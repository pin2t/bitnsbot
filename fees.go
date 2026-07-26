package main

import "math"
import "sort"

// Fee estimation follows mempool.space's algorithm rather than Core's
// `estimatesmartfee`, because the two answer different questions and Core's
// answer is routinely far too low during a fee spike.
//
// Core's estimator is **backward-looking**: it records how long transactions at
// each fee rate historically took to confirm and answers "what rate has been
// getting confirmed within N blocks recently". It is deliberately conservative
// and slow to react, so when the mempool fills in minutes it keeps quoting
// yesterday's cheap rates and the transaction sticks.
//
// mempool.space is **forward-looking**: it takes the mempool as it stands right
// now, packs it into the blocks a miner would actually build, and reports what
// each of those blocks is paying. That reacts immediately, which is why its
// numbers run higher.
//
// Two stages, ported from mempool.space's backend (`api/mempool-blocks.ts`,
// `api/common.ts` and `api/fee-api.ts`):
//
//  1. Pack the mempool into projected blocks and take each block's "median fee",
//     which is not a plain median but the weighted average rate of the middle
//     0.25% of the block's weight.
//  2. Turn the first few blocks' medians into recommendations, blending each
//     with the one above it and clamping against the mempool's own minimum.
//
// The one deliberate approximation is in stage 1: mempool.space runs a full
// getblocktemplate with cluster linearisation, while this sorts by ancestor fee
// rate — which is what Core's own miner maximises, and identical for every
// transaction without unconfirmed parents. See buildProjectedBlocks.

// blockWeightUnits is a block's weight limit, and the unit projected blocks are
// packed in. mempool.space's BLOCK_WEIGHT_UNITS.
const blockWeightUnits = 4_000_000

// projectedBlocksAmount is how many blocks ahead the mempool is projected into,
// matching mempool.space's MEMPOOL_BLOCKS_AMOUNT. Only the first four are used
// for recommendations; the rest exist so the last one absorbs the overflow the
// same way theirs does.
const projectedBlocksAmount = 8

// minIncrement is the rounding granularity of a recommendation, in sat/vB.
// mempool.space uses 1 for Bitcoin (0.1 for Liquid).
const minIncrement = 1.0

// Note: mempool.space has a second, "precise" variant that adds a priority
// offset and rounds to 0.001 sat/vB. The public /api/v1/fees/recommended
// endpoint does not use it, and neither does this — verified by reproducing
// their published numbers exactly (see fees_test.go).

// mempoolTx is the little of a mempool entry that block packing needs.
type mempoolTx struct {
    weight int64
    rate   float64 // effective fee rate, sat/vB
}

// projectedBlock is one block a miner could build from the current mempool.
type projectedBlock struct {
    vsize     float64 // fractional (weight/4), to avoid rounding drift
    medianFee float64 // sat/vB, see medianFeeOf
}

// recommendedFees is the set mempool.space publishes at /api/v1/fees/recommended.
type recommendedFees struct {
    fastest  float64
    halfHour float64
    hour     float64
    economy  float64
    minimum  float64
}

// effectiveRate is the fee rate a miner effectively earns for including a
// transaction, in sat/vB. It uses the whole unconfirmed ancestor package, so a
// cheap parent dragged in by an expensive child (CPFP) is rated by the package
// rather than by itself — which is how Core's miner picks transactions too.
// With no unconfirmed parents the ancestor figures equal the transaction's own,
// so this reduces to fee ÷ vsize.
func effectiveRate(e coreMempoolEntry) float64 {
    if e.AncestorSize > 0 && e.Fees.Ancestor > 0 {
        return e.Fees.Ancestor * 1e8 / float64(e.AncestorSize)
    }
    if e.Vsize > 0 {
        return e.Fees.Base * 1e8 / float64(e.Vsize)
    }
    return 0
}

// buildProjectedBlocks packs the mempool into the blocks a miner would build:
// highest effective fee rate first, 4M weight units per block, until the mempool
// runs out or projectedBlocksAmount blocks are full. Everything left over is
// stacked into the final block, as mempool.space does — that overflow block is
// never used for a recommendation, only the first four are.
//
// mempool.space runs a real getblocktemplate here, with cluster linearisation so
// a CPFP package moves as a unit. Sorting by ancestor fee rate is the standard
// approximation of that and is exactly equal for any transaction with no
// unconfirmed parents, which is the overwhelming majority; it can misplace the
// interior of a long CPFP chain. Since the recommendation reads the *middle* of
// each block by weight, a few misplaced chain members do not move it.
func buildProjectedBlocks(entries map[string]coreMempoolEntry) []projectedBlock {
    var txs = make([]mempoolTx, 0, len(entries))
    for _, e := range entries {
        var weight = e.Weight
        if weight <= 0 { weight = int64(e.Vsize) * 4 } // older nodes omit weight
        if weight <= 0 { continue }
        txs = append(txs, mempoolTx{weight: weight, rate: effectiveRate(e)})
    }
    if len(txs) == 0 { return nil }
    sort.Slice(txs, func(i, j int) bool {
        if txs[i].rate != txs[j].rate { return txs[i].rate > txs[j].rate }
        return txs[i].weight < txs[j].weight
    })
    var blocks []projectedBlock
    var current []mempoolTx
    var currentWeight int64
    for _, t := range txs {
        // the last block takes everything remaining rather than starting a new one
        var last = len(blocks) == projectedBlocksAmount-1
        if !last && currentWeight+t.weight > blockWeightUnits && len(current) > 0 {
            blocks = append(blocks, finishBlock(current, currentWeight))
            current, currentWeight = nil, 0
        }
        current = append(current, t)
        currentWeight += t.weight
    }
    if len(current) > 0 {
        blocks = append(blocks, finishBlock(current, currentWeight))
    }
    return blocks
}

func finishBlock(txs []mempoolTx, weight int64) projectedBlock {
    return projectedBlock{vsize: float64(weight) / 4, medianFee: medianFeeOf(txs)}
}

// medianFeeOf is mempool.space's "median fee" for a projected block, which is
// not the median transaction's rate: it is the weight-weighted average rate of
// the middle 0.25% of the block's weight (10 000 weight units either side of the
// midpoint). Averaging a thin band rather than taking one transaction's rate
// keeps the figure from jumping around as individual transactions arrive.
//
// A block that is not full counts its unused space as the cheapest part of the
// block, so the sampled band lands at the *cheap end* of the real transactions
// rather than their middle. That is the right answer: if a block is only half
// full you barely have to outbid anything to get into it. (A 4M-weight block of
// rates 1..4000 samples 1996..2005; the same transactions in a half-empty block
// sample 1..5.)
func medianFeeOf(txs []mempoolTx) float64 {
    if len(txs) <= 1 { return 0 }
    var sorted = make([]mempoolTx, len(txs))
    copy(sorted, txs)
    sort.Slice(sorted, func(i, j int) bool { return sorted[i].rate < sorted[j].rate })
    var totalWeight int64
    for _, t := range sorted {
        totalWeight += t.weight
    }
    var weightCount = float64(blockWeightUnits - totalWeight)
    var halfWidth = float64(blockWeightUnits) / 800
    var leftBound = math.Floor(float64(blockWeightUnits)/2 - halfWidth)
    var rightBound = math.Ceil(float64(blockWeightUnits)/2 + halfWidth)
    var weightedFee, sampledWeight float64
    for i := 0; i < len(sorted) && weightCount < rightBound; i++ {
        var left = weightCount
        var right = weightCount + float64(sorted[i].weight)
        if right > leftBound {
            var w = math.Min(right, rightBound) - math.Max(left, leftBound)
            weightedFee += sorted[i].rate * (w / 4)
            sampledWeight += w
        }
        weightCount = right
    }
    if sampledWeight == 0 { return 0 }
    return weightedFee / (sampledWeight / 4)
}

// calculateRecommendedFee turns projected blocks into the published
// recommendations. mempoolMinFee is the node's own purge threshold in BTC/kvB
// (getmempoolinfo's mempoolminfee), which floors every answer: quoting below it
// would recommend a fee the node itself would not even relay.
func calculateRecommendedFee(blocks []projectedBlock, mempoolMinFee float64) recommendedFees {
    var purgeRate = roundUpTo(mempoolMinFee*1e5, minIncrement)
    var minimum = math.Max(purgeRate, minIncrement)
    if len(blocks) == 0 {
        return recommendedFees{minimum, minimum, minimum, minimum, minimum}
    }
    var at = func(i int) *projectedBlock {
        if i < len(blocks) { return &blocks[i] }
        return nil
    }
    // each tier blends with the tier above it, so the numbers step down smoothly
    // instead of cliff-edging between blocks
    var first = optimizeMedianFee(at(0), at(1), 0, minimum)
    var second = minimum
    if at(1) != nil { second = optimizeMedianFee(at(1), at(2), first, minimum) }
    var third = minimum
    if at(2) != nil { third = optimizeMedianFee(at(2), at(3), second, minimum) }
    var fastest = math.Max(minimum, first)
    var halfHour = math.Max(minimum, second)
    var hour = math.Max(minimum, third)
    var economy = math.Max(minimum, math.Min(2*minimum, third))
    // recommendations must never invert: paying for speed cannot cost less
    fastest = math.Max(fastest, math.Max(halfHour, math.Max(hour, economy)))
    halfHour = math.Max(halfHour, math.Max(hour, economy))
    hour = math.Max(hour, economy)
    return recommendedFees{
        fastest:  roundTo(fastest, minIncrement),
        halfHour: roundTo(halfHour, minIncrement),
        hour:     roundTo(hour, minIncrement),
        economy:  roundTo(economy, minIncrement),
        minimum:  roundTo(minimum, minIncrement),
    }
}

// optimizeMedianFee derives one tier from a projected block. previousFee is the
// faster tier's answer (0 for the first), which this averages with to smooth the
// step between tiers. A block less than half full is not evidence of congestion
// at all, so it falls back to the minimum; a block between half and 95% full is
// scaled down in proportion, since a partly-full block means the next one will
// clear anything at that rate.
func optimizeMedianFee(block, next *projectedBlock, previousFee, minFee float64) float64 {
    var useFee = block.medianFee
    if previousFee > 0 { useFee = (block.medianFee + previousFee) / 2 }
    if block.vsize <= 500_000 || block.medianFee < minFee {
        return minFee
    }
    if block.vsize <= 950_000 && next == nil {
        var multiplier = (block.vsize - 500_000) / 500_000
        return math.Max(roundTo(useFee*multiplier, minIncrement), minFee)
    }
    return math.Max(roundUpTo(useFee, minIncrement), minFee)
}

func roundUpTo(value, nearest float64) float64 {
    if nearest == 0 { return value }
    return math.Ceil(value/nearest) * nearest
}

func roundTo(value, nearest float64) float64 {
    if nearest == 0 { return value }
    return math.Round(value/nearest) * nearest
}
