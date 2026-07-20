package main

import "encoding/binary"
import "encoding/hex"
import "net"
import "os"
import "path/filepath"
import "reflect"
import "testing"
import "time"

// The empty verack message is the canonical Bitcoin wire test vector: magic,
// "verack" padded to 12 bytes, a zero length, and 5df6e0e2 — the first four bytes
// of the double SHA-256 of nothing.
func TestMessageEnvelope(t *testing.T) {
    var want = "f9beb4d976657261636b0000000000000000000" + "05df6e0e2"
    if got := hex.EncodeToString(message("verack", nil)); got != want {
        t.Fatalf("verack message = %s, want %s", got, want)
    }
    var msg = message("addr", []byte{0x01, 0x02})
    if got := binary.LittleEndian.Uint32(msg[16:20]); got != 2 {
        t.Fatalf("payload length = %d, want 2", got)
    }
    if string(msg[4:8]) != "addr" || msg[8] != 0 {
        t.Fatalf("command not null-padded: %q", msg[4:16])
    }
    if !reflect.DeepEqual(msg[24:], []byte{0x01, 0x02}) {
        t.Fatalf("payload not appended: %v", msg[24:])
    }
}

// The version payload is written by hand, so pin its layout: the fixed fields,
// the one-byte length prefix on the user agent, and relay=0 at the end.
func TestVersionPayload(t *testing.T) {
    var payload = versionPayload(net.ParseIP("1.2.3.4"), 8333)
    var uaAt = 4 + 8 + 8 + 26 + 26 + 8
    if len(payload) != uaAt+1+len(userAgent)+4+1 {
        t.Fatalf("payload is %d bytes, want %d", len(payload), uaAt+1+len(userAgent)+4+1)
    }
    if got := binary.LittleEndian.Uint32(payload[0:4]); got != protocolVersion {
        t.Fatalf("protocol version = %d, want %d", got, protocolVersion)
    }
    if got := binary.LittleEndian.Uint64(payload[4:12]); got != 0 {
        t.Fatalf("services = %d, want 0 — we serve nothing", got)
    }
    if payload[uaAt] != byte(len(userAgent)) {
        t.Fatalf("user agent length prefix = %d, want %d", payload[uaAt], len(userAgent))
    }
    if got := string(payload[uaAt+1 : uaAt+1+len(userAgent)]); got != userAgent {
        t.Fatalf("user agent = %q, want %q", got, userAgent)
    }
    if payload[len(payload)-1] != 0 {
        t.Fatal("relay must be 0 so the peer doesn't stream transactions at us")
    }
    // the address we tell the peer it is
    if got := net.IP(payload[28:44]); !got.Equal(net.ParseIP("1.2.3.4")) {
        t.Fatalf("addr_recv IP = %v, want 1.2.3.4", got)
    }
}

// The addr payload is one entry: the count, a timestamp, and the 26-byte network
// address carrying services, the IPv4-mapped IP and the port in network order.
func TestAddrPayload(t *testing.T) {
    var payload = addrPayload(net.ParseIP("178.46.128.227"), 8333)
    if len(payload) != 31 {
        t.Fatalf("payload is %d bytes, want 31", len(payload))
    }
    if payload[0] != 1 {
        t.Fatalf("address count = %d, want 1", payload[0])
    }
    var stamp = binary.LittleEndian.Uint32(payload[1:5])
    if delta := time.Since(time.Unix(int64(stamp), 0)); delta < 0 || delta > time.Minute {
        t.Fatalf("timestamp %d is not around now", stamp)
    }
    if got := binary.LittleEndian.Uint64(payload[5:13]); got != advertisedServices {
        t.Fatalf("services = %d, want %d", got, advertisedServices)
    }
    if got := net.IP(payload[13:29]); !got.Equal(net.ParseIP("178.46.128.227")) {
        t.Fatalf("advertised IP = %v, want 178.46.128.227", got)
    }
    if got := binary.BigEndian.Uint16(payload[29:31]); got != 8333 {
        t.Fatalf("port = %d, want 8333", got)
    }
}

// Only IPv4 peers are contacted: IPv6 needs a working route and .onion needs Tor,
// and peers.json is full of both.
func TestIPv4Peers(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "peers.json")
    var content = `{"Version":2,"Key":[1,2],"Addresses":[
        {"Addr":"151.38.1.18:8333","LastSuccess":-62135596800},
        {"Addr":"[2600:1702:62f0:34d0:4f4:e9d8:a8fc:d112]:8333","LastSuccess":1784530597},
        {"Addr":"p4lrlm4cg5gsmzdhnttx3xyak2ya4zvp4uvldz27c7kdkkncfbldtjyd.onion:8333","LastSuccess":1784530597},
        {"Addr":"149.40.50.220:9333","LastSuccess":1784530597},
        {"Addr":"garbage","LastSuccess":1784530597}
    ],"NewBuckets":[[1]],"TriedBuckets":[[2]]}`
    if err := os.WriteFile(path, []byte(content), 0600); err != nil {
        t.Fatalf("write: %v", err)
    }
    var got, err = ipv4Peers(path, false)
    if err != nil {
        t.Fatalf("ipv4Peers: %v", err)
    }
    var want = []string{"151.38.1.18:8333", "149.40.50.220:9333"}
    if !reflect.DeepEqual(got, want) {
        t.Fatalf("peers = %v, want %v", got, want)
    }
    // -live drops the address btcd has never managed to connect to; most of a
    // real peers.json is exactly that
    var live, lerr = ipv4Peers(path, true)
    if lerr != nil {
        t.Fatalf("ipv4Peers(live): %v", lerr)
    }
    if !reflect.DeepEqual(live, []string{"149.40.50.220:9333"}) {
        t.Fatalf("live peers = %v, want just the one btcd reached", live)
    }
}

// The whole exchange against a peer that speaks the protocol: it must see our
// version, then our verack once it sends its own version, and finally the addr
// message naming the address we are advertising.
func TestAnnounce(t *testing.T) {
    var ln, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen: %v", err)
    }
    defer ln.Close()
    var seen = make(chan string, 8)
    var addrPayloads = make(chan []byte, 1)
    go func() {
        var conn, err = ln.Accept()
        if err != nil { return }
        defer conn.Close()
        for {
            var command, payload, err = readMessage(conn)
            if err != nil { return }
            seen <- command
            switch command {
            case "version":
                conn.Write(message("sendaddrv2", nil)) // noise before verack, must be ignored
                conn.Write(message("version", versionPayload(net.IPv4(127, 0, 0, 1), 0)))
                conn.Write(message("verack", nil))
            case "addr":
                addrPayloads <- payload
                return
            }
        }
    }()
    var ip, port, serr = splitAddr(ln.Addr().String())
    if serr != nil {
        t.Fatalf("split listener address: %v", serr)
    }
    if err := announce(ln.Addr().String(), ip, port, net.ParseIP("178.46.128.227"), 8333); err != nil {
        t.Fatalf("announce: %v", err)
    }
    select {
    case payload := <-addrPayloads:
        if got := net.IP(payload[13:29]); !got.Equal(net.ParseIP("178.46.128.227")) {
            t.Fatalf("peer was told about %v, want 178.46.128.227", got)
        }
        if got := binary.BigEndian.Uint16(payload[29:31]); got != 8333 {
            t.Fatalf("peer was told port %d, want 8333", got)
        }
    case <-time.After(10 * time.Second):
        t.Fatal("the peer never received an addr message")
    }
    var order []string
    for len(seen) > 0 {
        order = append(order, <-seen)
    }
    if !reflect.DeepEqual(order, []string{"version", "verack", "addr"}) {
        t.Fatalf("peer saw %v, want version, verack, addr", order)
    }
}

// A peer that never finishes the handshake must not hang the worker.
func TestAnnounceTimesOut(t *testing.T) {
    var ln, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen: %v", err)
    }
    defer ln.Close()
    go func() {
        var conn, err = ln.Accept()
        if err != nil { return }
        defer conn.Close()
        select {} // accept and say nothing
    }()
    var saved = *timeout
    *timeout = 300 * time.Millisecond
    defer func() { *timeout = saved }()
    var ip, port, _ = splitAddr(ln.Addr().String())
    var began = time.Now()
    if err := announce(ln.Addr().String(), ip, port, net.ParseIP("178.46.128.227"), 8333); err == nil {
        t.Fatal("expected a timeout error from a silent peer")
    }
    if elapsed := time.Since(began); elapsed > 5*time.Second {
        t.Fatalf("announce took %s against a silent peer", elapsed)
    }
}
