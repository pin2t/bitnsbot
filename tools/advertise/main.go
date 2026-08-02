// Command advertise announces a Bitcoin node address to the peers btcd knows
// about. It reads btcd's peers.json, and for every IPv4 peer in it performs the
// P2P version/verack handshake and sends an addr message naming the address to
// advertise, which is how a node's address spreads through the gossip network.
//
// The wire format is built by hand on the standard library — the handshake needs
// only three message types, which is far less than pulling in a full node's wire
// package (and matches how the rest of this repo treats dependencies).
package main

import "bytes"
import "crypto/rand"
import "crypto/sha256"
import "encoding/binary"
import "encoding/json"
import "flag"
import "fmt"
import "io"
import "net"
import "os"
import "strconv"
import "strings"
import "sync"
import "sync/atomic"
import "time"

import "bitnsbot/logging"

var peersPath = flag.String("peers", "", "path to btcd's peers.json (optional)")
var corePeersPath = flag.String("corepeers", "", "path to Bitcoin Core's peers.dat (optional)")
var advertised = flag.String("advertise", "5.181.105.56:8333", "the address to advertise to every peer")
var workers = flag.Int("workers", 64, "how many peers to contact concurrently")
var timeout = flag.Duration("timeout", 10*time.Second, "dial and I/O timeout per peer")
var limit = flag.Int("limit", 0, "contact at most this many peers (0 = all of them)")
var liveOnly = flag.Bool("live", false, "only contact peers btcd has successfully connected to before; most of peers.json is unverified gossip that never answers")
var verbose = flag.Int("verbose", 0, "log verbosity: 0=summary and errors, 1=+one line per peer")

// mainnetMagic prefixes every message on Bitcoin mainnet; a peer drops anything
// that starts with anything else.
const mainnetMagic = 0xd9b4bef9

const protocolVersion = 70016
const userAgent = "/bitnsbot-advertise:0.1/"

// advertisedServices is what we claim the advertised node offers — NODE_NETWORK
// (a full node serving blocks) plus NODE_WITNESS. Peers weigh this when deciding
// whether to keep and relay an address.
const advertisedServices = 1 | 8

// maxPayload caps what we will read from a peer. Nothing in a handshake is large;
// this only exists so a hostile peer can't announce a huge length and make us
// allocate for it.
const maxPayload = 4 << 20

// message wraps a payload in the Bitcoin message envelope: magic, the 12-byte
// null-padded command name, the payload length, and the first four bytes of the
// double SHA-256 of the payload as a checksum.
func message(command string, payload []byte) []byte {
    var buf = make([]byte, 24+len(payload))
    binary.LittleEndian.PutUint32(buf[0:4], mainnetMagic)
    copy(buf[4:16], command)
    binary.LittleEndian.PutUint32(buf[16:20], uint32(len(payload)))
    var first = sha256.Sum256(payload)
    var second = sha256.Sum256(first[:])
    copy(buf[20:24], second[:4])
    copy(buf[24:], payload)
    return buf
}

// netAddress encodes the 26-byte network address used inside a version message:
// services, the IP as IPv4-mapped IPv6, and the port in network byte order.
func netAddress(services uint64, ip net.IP, port uint16) []byte {
    var b = make([]byte, 26)
    binary.LittleEndian.PutUint64(b[0:8], services)
    copy(b[8:24], ip.To16())
    binary.BigEndian.PutUint16(b[24:26], port)
    return b
}

func nonce() uint64 {
    var b [8]byte
    rand.Read(b[:])
    return binary.LittleEndian.Uint64(b[:])
}

// versionPayload builds our side of the handshake. We claim no services (we serve
// nothing) and set relay=0 so the peer doesn't start streaming transactions at a
// connection we are about to close. The user agent is length-prefixed by a
// varint, but only its one-byte form is written: the general encoding starts at
// 0xfd, and this string is 24 characters.
func versionPayload(remote net.IP, port uint16) []byte {
    var b = new(bytes.Buffer)
    binary.Write(b, binary.LittleEndian, int32(protocolVersion))
    binary.Write(b, binary.LittleEndian, uint64(0))
    binary.Write(b, binary.LittleEndian, time.Now().Unix())
    b.Write(netAddress(0, remote, port))
    b.Write(netAddress(0, net.IPv4zero, 0))
    binary.Write(b, binary.LittleEndian, nonce())
    b.WriteByte(byte(len(userAgent)))
    b.WriteString(userAgent)
    binary.Write(b, binary.LittleEndian, int32(0))
    b.WriteByte(0)
    return b.Bytes()
}

// addrPayload builds a one-entry addr message: the count — again a varint whose
// one-byte form is all a count of 1 needs — then the timestamp the address was
// last seen (now) followed by the same 26-byte network address.
func addrPayload(ip net.IP, port uint16) []byte {
    var b = new(bytes.Buffer)
    b.WriteByte(1)
    binary.Write(b, binary.LittleEndian, uint32(time.Now().Unix()))
    b.Write(netAddress(advertisedServices, ip, port))
    return b.Bytes()
}

func readMessage(conn net.Conn) (command string, payload []byte, err error) {
    var header [24]byte
    if _, err := io.ReadFull(conn, header[:]); err != nil { return "", nil, err }
    if binary.LittleEndian.Uint32(header[0:4]) != mainnetMagic {
        return "", nil, fmt.Errorf("message with wrong network magic")
    }
    var length = binary.LittleEndian.Uint32(header[16:20])
    if length > maxPayload {
        return "", nil, fmt.Errorf("oversized %d-byte payload", length)
    }
    payload = make([]byte, length)
    if _, err := io.ReadFull(conn, payload); err != nil { return "", nil, err }
    return strings.TrimRight(string(header[4:16]), "\x00"), payload, nil
}

// announce completes the handshake with one peer and sends it the addr message.
// The handshake is done once both sides' version and verack have been exchanged;
// anything else the peer sends first (sendaddrv2, wtxidrelay, sendheaders …) is
// read and ignored.
func announce(peer string, ip net.IP, port uint16, advIP net.IP, advPort uint16) error {
    var conn, err = net.DialTimeout("tcp", peer, *timeout)
    if err != nil { return err }
    defer conn.Close()
    if err := conn.SetDeadline(time.Now().Add(*timeout)); err != nil { return err }
    if _, err := conn.Write(message("version", versionPayload(ip, port))); err != nil { return err }
    var gotVersion, gotVerack bool
    for !gotVersion || !gotVerack {
        var command, _, err = readMessage(conn)
        if err != nil { return err }
        switch command {
        case "version":
            gotVersion = true
            if _, err := conn.Write(message("verack", nil)); err != nil { return err }
        case "verack":
            gotVerack = true
        }
    }
    if _, err := conn.Write(message("addr", addrPayload(advIP, advPort))); err != nil { return err }
    return nil
}

// neverConnected is the LastSuccess/LastAttempt btcd writes for an address it has
// never reached: Go's zero time in Unix seconds.
const neverConnected = -62135596800

// ipv4Peers reads btcd's peers.json and returns the IPv4 peer addresses in it.
// The file also holds IPv6 and .onion entries, which this tool skips: reaching
// those needs a working IPv6 route or a Tor proxy respectively. With liveOnly it
// further keeps only addresses btcd has actually connected to — most of the file
// is unverified gossip that btcd itself has never dialled, so a full run spends
// most of its time waiting for dead hosts to time out.
func ipv4Peers(path string, liveOnly bool) ([]string, error) {
    var data, err = os.ReadFile(path)
    if err != nil { return nil, err }
    var file struct {
        Addresses []struct {
            Addr        string
            LastSuccess int64
        }
    }
    if err := json.Unmarshal(data, &file); err != nil { return nil, err }
    var peers []string
    for _, a := range file.Addresses {
        if liveOnly && a.LastSuccess == neverConnected { continue }
        if ip, _, err := splitAddr(a.Addr); err == nil && ip.To4() != nil {
            peers = append(peers, a.Addr)
        }
    }
    return peers, nil
}

// corePeers reads Bitcoin Core's peers.dat and returns the IPv4 peer addresses in
// it. peers.dat is a flat binary file: a 32-byte addrman key, followed by the
// counts of new and tried entries (uint32 LE), then each entry as a 34-byte
// record — version (int32), time (uint32), services (uint64), the 16-byte
// IPv4-mapped IPv6 address, and the port in network byte order. Tried entries
// may carry extra bookkeeping after the address, but those bytes are never
// needed to extract the IP:port.
func corePeers(path string) ([]string, error) {
    var data, err = os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    if len(data) < 40 { // key (32) + nNew (4) + nTried (4)
        return nil, fmt.Errorf("%s: too small for a peers.dat (%d bytes)", path, len(data))
    }
    var nNew = binary.LittleEndian.Uint32(data[32:36])
    var nTried = binary.LittleEndian.Uint32(data[36:40])
    var total = int(nNew + nTried)
    const entrySize = 34
    var off = 40
    var peers []string
    for i := 0; i < total; i++ {
        if off+entrySize > len(data) {
            break
        }
        var ip = net.IP(data[off+16 : off+32])
        var port = binary.BigEndian.Uint16(data[off+32 : off+34])
        off += entrySize
        if ip4 := ip.To4(); ip4 != nil {
            peers = append(peers, net.JoinHostPort(ip4.String(), strconv.Itoa(int(port))))
        }
    }
    return peers, nil
}

func splitAddr(addr string) (net.IP, uint16, error) {
    var host, portText, err = net.SplitHostPort(addr)
    if err != nil { return nil, 0, err }
    var ip = net.ParseIP(host)
    if ip == nil { return nil, 0, fmt.Errorf("%q is not an IP address", host) }
    var port, perr = strconv.ParseUint(portText, 10, 16)
    if perr != nil { return nil, 0, perr }
    return ip, uint16(port), nil
}

func main() {
    flag.Parse()
    logging.SetVerbose(*verbose)
    var advIP, advPort, err = splitAddr(*advertised)
    if err != nil {
        logging.Fatal("-advertise %q: %v", *advertised, err)
    }
    if *peersPath == "" && *corePeersPath == "" {
        logging.Fatal("at least one of -peers or -corepeers is required")
    }
    var peers []string
    var from []string
    if *peersPath != "" {
        var p, err = ipv4Peers(*peersPath, *liveOnly)
        if err != nil {
            logging.Fatal("read %s: %v", *peersPath, err)
        }
        peers = p
        from = append(from, *peersPath)
    }
    if *corePeersPath != "" {
        var dps, derr = corePeers(*corePeersPath)
        if derr != nil {
            logging.Fatal("read %s: %v", *corePeersPath, derr)
        }
        var seen = make(map[string]bool, len(peers)+len(dps))
        for _, p := range peers {
            seen[p] = true
        }
        for _, p := range dps {
            if !seen[p] {
                seen[p] = true
                peers = append(peers, p)
            }
        }
        from = append(from, *corePeersPath)
    }
    if *limit > 0 && len(peers) > *limit {
        peers = peers[:*limit]
    }
    logging.Status("advertising %s to %d IPv4 peers from %s", *advertised, len(peers), strings.Join(from, ", "))
    var announced, failed atomic.Int64
    var began = time.Now()
    var wg sync.WaitGroup
    var sem = make(chan struct{}, *workers)
    for _, peer := range peers {
        wg.Add(1)
        sem <- struct{}{}
        go func(peer string) {
            defer wg.Done()
            defer func() { <-sem }()
            var ip, port, err = splitAddr(peer)
            if err != nil {
                failed.Add(1)
                return
            }
            if err := announce(peer, ip, port, advIP, advPort); err != nil {
                failed.Add(1)
                logging.Info("%s: %v", peer, err)
                return
            }
            announced.Add(1)
            logging.Info("%s: advertised", peer)
        }(peer)
    }
    wg.Wait()
    logging.Status("advertised to %d peers, %d unreachable, in %s",
        announced.Load(), failed.Load(), time.Since(began).Round(time.Second))
}
