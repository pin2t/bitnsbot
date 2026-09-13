package signals

import "sync"
import "testing"

func TestFireWakesEverySubscriber(t *testing.T) {
    Reset()
    var a, b = Subscribe(Block), Subscribe(Block)
    var other = Subscribe(AddrStat)
    Send(Block)
    for i, ch := range []<-chan struct{}{a, b} {
        select {
        case <-ch:
        default:
            t.Errorf("subscriber %d was not woken", i)
        }
    }
    // and only those waiting on that name
    select {
    case <-other:
        t.Error("a signal reached a subscriber of another name")
    default:
    }
}

// The channel holds one pending signal: a scan busy through three blocks wakes
// once more when it finishes, not three times over the same catch-up. And a fire
// must never block the handler that makes it — which is a ZMQ read loop.
func TestFireCoalescesAndNeverBlocks(t *testing.T) {
    Reset()
    var ch = Subscribe(Block)
    for i := 0; i < 100; i++ { Send(Block) }
    select {
    case <-ch:
    default:
        t.Fatal("nothing pending after 100 fires")
    }
    select {
    case <-ch:
        t.Error("a second signal was queued; one pending is the contract")
    default:
    }
}

func TestFireWithNobodyListening(t *testing.T) {
    Reset()
    Send(Block)
    Send("nobody has this one")
}

// Fired from the ZMQ read loop while subscribers are starting up, so both sides
// are locked.
func TestConcurrentFireAndSubscribe(t *testing.T) {
    Reset()
    var wg sync.WaitGroup
    for i := 0; i < 50; i++ {
        wg.Add(2)
        go func() { defer wg.Done(); Subscribe(Block) }()
        go func() { defer wg.Done(); Send(Block) }()
    }
    wg.Wait()
}
