package txwatches

import "testing"

func TestConfirmsConsumes(t *testing.T) {
    Reset()
    defer Reset()
    Add("tx1", "tg", 1, "a")
    AddAddrConfirm("tx1", "tg", 2, "addrX", "b", Summary{})
    if c := Confirms([]string{"tx1", "txNone"}); len(c) != 2 {
        t.Fatalf("expected 2 confirmed, got %d", len(c))
    }
    if len(Confirms([]string{"tx1"})) != 0 {
        t.Fatalf("Confirms should have removed the watches")
    }
}

// exact duplicate
//
// distinct: direct watch (addr "")
func TestDedup(t *testing.T) {
    Reset()
    defer Reset()
    AddAddrConfirm("tx", "tg", 1, "addrX", "a", Summary{})
    AddAddrConfirm("tx", "tg", 1, "addrX", "a", Summary{})
    Add("tx", "tg", 1, "")
    if n := len(Confirms([]string{"tx"})); n != 2 {
        t.Fatalf("expected 2 (deduped addr-confirm + direct), got %d", n)
    }
}

// direct
//
// address-derived
func TestRemoveDirectOnly(t *testing.T) {
    Reset()
    defer Reset()
    Add("tx", "tg", 1, "")
    AddAddrConfirm("tx", "tg", 1, "addrX", "a", Summary{})
    if r := Remove("tx", "tg", 1); r != 1 {
        t.Fatalf("expected 1 direct removed, got %d", r)
    }
    var c = Confirms([]string{"tx"})
    if len(c) != 1 || c[0].Addr != "addrX" {
        t.Fatalf("address-derived watch should survive Remove: %#v", c)
    }
}

// direct, unaffected
func TestRemoveAddrConfirms(t *testing.T) {
    Reset()
    defer Reset()
    AddAddrConfirm("tx1", "tg", 1, "addrX", "a", Summary{})
    AddAddrConfirm("tx2", "tg", 1, "addrX", "b", Summary{})
    Add("tx3", "tg", 1, "")
    RemoveAddrConfirms("addrX", "tg", 1)
    if len(Confirms([]string{"tx1", "tx2"})) != 0 {
        t.Fatalf("addrX confirmations should be gone")
    }
    if len(Confirms([]string{"tx3"})) != 1 {
        t.Fatalf("direct watch should remain")
    }
}

// address-derived, not listed
//
// other chat
func TestForDirectOnly(t *testing.T) {
    Reset()
    defer Reset()
    Add("txA", "tg", 1, "alias-a")
    AddAddrConfirm("txB", "tg", 1, "addrX", "alias-b", Summary{})
    Add("txC", "tg", 2, "")
    var entries = For("tg", 1)
    if len(entries) != 1 || entries[0].Txid != "txA" || entries[0].Alias != "alias-a" {
        t.Fatalf("For(tg, 1) should list only the chat's direct watch: %#v", entries)
    }
}

// A Telegram chat and a MAX chat with the same number are different chats: one
// neither lists, removes nor renames the other's watches, and a confirmation
// says which platform it is for.
func TestPlatformsAreSeparate(t *testing.T) {
    Reset()
    defer Reset()
    Add("tx", "tg", 1, "telegram")
    Add("tx", "max", 1, "max")
    if e := For("max", 1); len(e) != 1 || e[0].Alias != "max" { t.Fatalf("For(max) = %#v", e) }
    if n := SetAlias("tx", "max", 1, "renamed"); n != 1 { t.Fatalf("SetAlias(max) = %d", n) }
    if n := Remove("tx", "tg", 1); n != 1 { t.Fatalf("Remove(tg) = %d", n) }
    var c = Confirms([]string{"tx"})
    if len(c) != 1 || c[0].Platform != "max" || c[0].Alias != "renamed" { t.Fatalf("Confirms = %#v", c) }
}
