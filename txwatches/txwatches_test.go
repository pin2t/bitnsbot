package txwatches

import "testing"

func TestConfirmsConsumes(t *testing.T) {
    Reset()
    defer Reset()
    Add("tx1", 1, "a")
    AddAddrConfirm("tx1", 2, "addrX", "b")
    if c := Confirms([]string{"tx1", "txNone"}); len(c) != 2 {
        t.Fatalf("expected 2 confirmed, got %d", len(c))
    }
    if len(Confirms([]string{"tx1"})) != 0 {
        t.Fatalf("Confirms should have removed the watches")
    }
}

func TestDedup(t *testing.T) {
    Reset()
    defer Reset()
    AddAddrConfirm("tx", 1, "addrX", "a")
    AddAddrConfirm("tx", 1, "addrX", "a") // exact duplicate
    Add("tx", 1, "")                      // distinct: direct watch (addr "")
    if n := len(Confirms([]string{"tx"})); n != 2 {
        t.Fatalf("expected 2 (deduped addr-confirm + direct), got %d", n)
    }
}

func TestRemoveDirectOnly(t *testing.T) {
    Reset()
    defer Reset()
    Add("tx", 1, "")                      // direct
    AddAddrConfirm("tx", 1, "addrX", "a") // address-derived
    if r := Remove("tx", 1); r != 1 {
        t.Fatalf("expected 1 direct removed, got %d", r)
    }
    var c = Confirms([]string{"tx"})
    if len(c) != 1 || c[0].Addr != "addrX" {
        t.Fatalf("address-derived watch should survive Remove: %#v", c)
    }
}

func TestRemoveAddrConfirms(t *testing.T) {
    Reset()
    defer Reset()
    AddAddrConfirm("tx1", 1, "addrX", "a")
    AddAddrConfirm("tx2", 1, "addrX", "b")
    Add("tx3", 1, "") // direct, unaffected
    RemoveAddrConfirms("addrX", 1)
    if len(Confirms([]string{"tx1", "tx2"})) != 0 {
        t.Fatalf("addrX confirmations should be gone")
    }
    if len(Confirms([]string{"tx3"})) != 1 {
        t.Fatalf("direct watch should remain")
    }
}

func TestForDirectOnly(t *testing.T) {
    Reset()
    defer Reset()
    Add("txA", 1, "alias-a")
    AddAddrConfirm("txB", 1, "addrX", "alias-b") // address-derived, not listed
    Add("txC", 2, "")                            // other chat
    var entries = For(1)
    if len(entries) != 1 || entries[0].Txid != "txA" || entries[0].Alias != "alias-a" {
        t.Fatalf("For(1) should list only the chat's direct watch: %#v", entries)
    }
}
