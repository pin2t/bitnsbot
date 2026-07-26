package main

import "math"
import "testing"

// The recommendation stage is a port of mempool.space's fee-api.ts, so it can be
// checked exactly: feed it the projected blocks *they* published and it must
// reproduce the recommendation they published at the same moment. Captured live
// from /api/v1/fees/mempool-blocks and /api/v1/fees/recommended.
func TestRecommendationMatchesMempoolSpace(t *testing.T) {
    var blocks = []projectedBlock{
        {vsize: 997962, medianFee: 3.011},
        {vsize: 997958, medianFee: 1.197},
        {vsize: 997986, medianFee: 0.490},
        {vsize: 997902, medianFee: 0.439},
        {vsize: 997994, medianFee: 0.375},
    }
    // their minimumFee was 1, i.e. a purge rate at or below 1 sat/vB
    var got = calculateRecommendedFee(blocks, 0.00001)
    var want = recommendedFees{fastest: 4, halfHour: 3, hour: 1, economy: 1, minimum: 1}
    if got != want {
        t.Fatalf("got %+v, want %+v (mempool.space published fastest=4 halfHour=3 hour=1 economy=1 minimum=1)", got, want)
    }
}

// Walking the same fixture by hand: block 0's median rounds up to 4; block 1
// blends with that (1.197+4)/2 = 2.6 → 3; block 2's median of 0.49 is under the
// 1 sat/vB floor, so it drops to the minimum. Pinned separately so a regression
// says which stage broke.
func TestRecommendationTierBlending(t *testing.T) {
    var blocks = []projectedBlock{
        {vsize: 997962, medianFee: 3.011},
        {vsize: 997958, medianFee: 1.197},
        {vsize: 997986, medianFee: 0.490},
        {vsize: 997902, medianFee: 0.439},
    }
    if got := optimizeMedianFee(&blocks[0], &blocks[1], 0, 1); got != 4 {
        t.Errorf("first tier = %v, want 4 (ceil of 3.011)", got)
    }
    if got := optimizeMedianFee(&blocks[1], &blocks[2], 4, 1); got != 3 {
        t.Errorf("second tier = %v, want 3 (ceil of (1.197+4)/2)", got)
    }
    if got := optimizeMedianFee(&blocks[2], &blocks[3], 3, 1); got != 1 {
        t.Errorf("third tier = %v, want the 1 sat/vB floor (its median is under it)", got)
    }
}

// An empty mempool has nothing to project from, so every tier is the node's own
// purge rate rather than an error.
func TestRecommendationEmptyMempool(t *testing.T) {
    var got = calculateRecommendedFee(nil, 0.00002)
    if got.fastest != 2 || got.minimum != 2 {
        t.Fatalf("got %+v, want every tier at the 2 sat/vB purge rate", got)
    }
}

// A recommendation must never invert — paying for the next block cannot cost
// less than waiting an hour, whatever the projected blocks look like.
func TestRecommendationNeverInverts(t *testing.T) {
    var cases = [][]projectedBlock{
        {{vsize: 999000, medianFee: 1.0}, {vsize: 999000, medianFee: 50.0}, {vsize: 999000, medianFee: 90.0}, {vsize: 999000, medianFee: 5}},
        {{vsize: 999000, medianFee: 80}, {vsize: 999000, medianFee: 2}, {vsize: 999000, medianFee: 1}, {vsize: 10, medianFee: 1}},
        {{vsize: 600000, medianFee: 30}},
    }
    for i, blocks := range cases {
        var r = calculateRecommendedFee(blocks, 0.00001)
        if r.fastest < r.halfHour || r.halfHour < r.hour || r.hour < r.economy || r.economy < r.minimum {
            t.Errorf("case %d inverted: %+v", i, r)
        }
    }
}

// A block under half full is not evidence of congestion, so its tier falls back
// to the floor however expensive its contents look.
func TestHalfEmptyBlockIsNotCongestion(t *testing.T) {
    var quiet = projectedBlock{vsize: 400_000, medianFee: 75}
    if got := optimizeMedianFee(&quiet, nil, 0, 1); got != 1 {
        t.Fatalf("a 40%%-full block gave %v, want the 1 sat/vB floor", got)
    }
}

// medianFeeOf is not the median transaction's rate: it is the weighted average
// of the middle 0.25% of the block's *weight*. With a full block of uniform
// transactions that lands on the rate at the halfway mark.
func TestMedianFeeOfSamplesTheMiddle(t *testing.T) {
    // 4000 transactions of 1000 weight each = 4M weight, exactly one full block.
    // Rates ascend 1..4000, so the middle by weight is around 2000.
    var txs []mempoolTx
    for i := 1; i <= 4000; i++ {
        txs = append(txs, mempoolTx{weight: 1000, rate: float64(i)})
    }
    var got = medianFeeOf(txs)
    if math.Abs(got-2000.5) > 5 {
        t.Fatalf("median = %v, want ≈2000 (the middle of an ascending 1..4000 block)", got)
    }
    // one transaction cannot have a meaningful middle
    if got := medianFeeOf([]mempoolTx{{weight: 1000, rate: 5}}); got != 0 {
        t.Fatalf("single-transaction block = %v, want 0", got)
    }
}

// Unused space counts as the cheapest part of the block, so a half-full block
// samples the cheap end of its transactions rather than their middle — you
// barely need to outbid anything to get into a block that is not full. The same
// transactions packed into a full block sample their true middle instead.
func TestMedianFeeOfAccountsForEmptySpace(t *testing.T) {
    var half []mempoolTx
    for i := 1; i <= 2000; i++ {
        half = append(half, mempoolTx{weight: 1000, rate: float64(i)})
    }
    if got := medianFeeOf(half); got > 10 {
        t.Fatalf("half-full block median = %v; empty space should drop the sample to the cheapest few (~3)", got)
    }
    // and a full block of the same shape samples its middle, far higher
    var full []mempoolTx
    for i := 1; i <= 4000; i++ {
        full = append(full, mempoolTx{weight: 1000, rate: float64(i)})
    }
    if got := medianFeeOf(full); got < 1500 {
        t.Fatalf("full block median = %v, want ≈2000", got)
    }
}

// Packing fills blocks highest-rate-first up to the weight limit, so the first
// projected block holds the most valuable transactions.
func TestBuildProjectedBlocksPacksByRate(t *testing.T) {
    var entries = map[string]coreMempoolEntry{}
    // 8000 transactions of 1000 weight = 8M weight = two full blocks
    for i := 0; i < 8000; i++ {
        var e = coreMempoolEntry{Vsize: 250, Weight: 1000}
        e.Fees.Base = float64(i+1) * 250 / 1e8 // rate = i+1 sat/vB
        e.Fees.Ancestor = e.Fees.Base
        e.AncestorSize = 250
        entries[string(rune(i))+"-"+trimNum(float64(i), 0)] = e
    }
    var blocks = buildProjectedBlocks(entries)
    if len(blocks) != 2 {
        t.Fatalf("got %d blocks, want 2 from 8M weight of transactions", len(blocks))
    }
    if blocks[0].vsize != 1_000_000 || blocks[1].vsize != 1_000_000 {
        t.Fatalf("block vsizes = %v, %v; want 1M each", blocks[0].vsize, blocks[1].vsize)
    }
    // the expensive half goes in the first block, so its median must be higher
    if blocks[0].medianFee <= blocks[1].medianFee {
        t.Fatalf("block 0 median %v is not above block 1's %v — packing is not rate-ordered",
            blocks[0].medianFee, blocks[1].medianFee)
    }
}

// A transaction dragged in by a high-fee child is rated by the whole package,
// not by its own cheap rate — otherwise CPFP would be invisible to the estimate.
func TestEffectiveRateUsesAncestorPackage(t *testing.T) {
    var cheapParent = coreMempoolEntry{Vsize: 200, Weight: 800}
    cheapParent.Fees.Base = 200 / 1e8    // 1 sat/vB alone
    cheapParent.Fees.Ancestor = 200 / 1e8
    cheapParent.AncestorSize = 200
    if got := effectiveRate(cheapParent); math.Abs(got-1) > 0.001 {
        t.Errorf("standalone rate = %v, want 1 sat/vB", got)
    }
    // a child paying for both: 400 vB package, 20 000 sats
    var child = coreMempoolEntry{Vsize: 200, Weight: 800}
    child.Fees.Base = 19800 / 1e8
    child.Fees.Ancestor = 20000 / 1e8
    child.AncestorSize = 400
    if got := effectiveRate(child); math.Abs(got-50) > 0.001 {
        t.Errorf("package rate = %v, want 50 sat/vB (20 000 sats over 400 vB)", got)
    }
}

// Nodes that omit weight (or entries with none) must not vanish from the
// projection — vsize×4 is the right fallback.
func TestBuildProjectedBlocksFallsBackToVsize(t *testing.T) {
    var entries = map[string]coreMempoolEntry{}
    for i := 0; i < 10; i++ {
        var e = coreMempoolEntry{Vsize: 250} // no Weight
        e.Fees.Base = 2500 / 1e8
        entries[trimNum(float64(i), 0)+"x"] = e
    }
    var blocks = buildProjectedBlocks(entries)
    if len(blocks) != 1 || blocks[0].vsize != 2500 {
        t.Fatalf("blocks = %+v, want one block of 2500 vB derived from vsize", blocks)
    }
}
