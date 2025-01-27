// Copyright 2017 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"math"
	"net/url"
	"strconv"

	"github.com/montanaflynn/stats"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/typeutil"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"github.com/tikv/pd/server/statistics"
	"go.uber.org/zap"
)

const (
	// adjustRatio is used to adjust TolerantSizeRatio according to region count.
	adjustRatio             float64 = 0.005
	leaderTolerantSizeRatio float64 = 5.0
	minTolerantSizeRatio    float64 = 1.0
	influenceAmp            int64   = 100
)

type balancePlan struct {
	kind              core.ScheduleKind
	cluster           opt.Cluster
	opInfluence       operator.OpInfluence
	tolerantSizeRatio float64

	source *core.StoreInfo
	target *core.StoreInfo
	region *core.RegionInfo

	sourceScore float64
	targetScore float64
}

func newBalancePlan(kind core.ScheduleKind, cluster opt.Cluster, opInfluence operator.OpInfluence) *balancePlan {
	return &balancePlan{
		kind:              kind,
		cluster:           cluster,
		opInfluence:       opInfluence,
		tolerantSizeRatio: adjustTolerantRatio(cluster, kind),
	}
}

func (p *balancePlan) GetOpInfluence(storeID uint64) int64 {
	return p.opInfluence.GetStoreInfluence(storeID).ResourceProperty(p.kind)
}

func (p *balancePlan) SourceStoreID() uint64 {
	return p.source.GetID()
}

func (p *balancePlan) SourceMetricLabel() string {
	return strconv.FormatUint(p.SourceStoreID(), 10)
}

func (p *balancePlan) TargetStoreID() uint64 {
	return p.target.GetID()
}

func (p *balancePlan) TargetMetricLabel() string {
	return strconv.FormatUint(p.TargetStoreID(), 10)
}

func (p *balancePlan) shouldBalance(scheduleName string) bool {
	// The reason we use max(regionSize, averageRegionSize) to check is:
	// 1. prevent moving small regions between stores with close scores, leading to unnecessary balance.
	// 2. prevent moving huge regions, leading to over balance.
	sourceID := p.source.GetID()
	targetID := p.target.GetID()
	tolerantResource := p.getTolerantResource()
	// to avoid schedule too much, if A's core greater than B and C a little
	// we want that A should be moved out one region not two
	sourceInfluence := p.GetOpInfluence(sourceID)
	// A->B, B's influence is positive , so B can become source schedule, it will move region from B to C
	if sourceInfluence > 0 {
		sourceInfluence = -sourceInfluence
	}
	// to avoid schedule too much, if A's score less than B and C in small range,
	// we want that A can be moved in one region not two
	targetInfluence := p.GetOpInfluence(targetID)
	// to avoid schedule call back
	// A->B, A's influence is negative，so A will be target,C may move region to A
	if targetInfluence < 0 {
		targetInfluence = -targetInfluence
	}
	opts := p.cluster.GetOpts()
	switch p.kind.Resource {
	case core.LeaderKind:
		sourceDelta, targetDelta := sourceInfluence-tolerantResource, targetInfluence+tolerantResource
		p.sourceScore = p.source.LeaderScore(p.kind.Policy, sourceDelta)
		p.targetScore = p.target.LeaderScore(p.kind.Policy, targetDelta)
	case core.RegionKind:
		sourceDelta, targetDelta := sourceInfluence*influenceAmp-tolerantResource, targetInfluence*influenceAmp+tolerantResource
		p.sourceScore = p.source.RegionScore(opts.GetRegionScoreFormulaVersion(), opts.GetHighSpaceRatio(), opts.GetLowSpaceRatio(), sourceDelta)
		p.targetScore = p.target.RegionScore(opts.GetRegionScoreFormulaVersion(), opts.GetHighSpaceRatio(), opts.GetLowSpaceRatio(), targetDelta)
	}
	if opts.IsDebugMetricsEnabled() {
		opInfluenceStatus.WithLabelValues(scheduleName, strconv.FormatUint(sourceID, 10), "source").Set(float64(sourceInfluence))
		opInfluenceStatus.WithLabelValues(scheduleName, strconv.FormatUint(targetID, 10), "target").Set(float64(targetInfluence))
		tolerantResourceStatus.WithLabelValues(scheduleName, strconv.FormatUint(sourceID, 10), strconv.FormatUint(targetID, 10)).Set(float64(tolerantResource))
	}
	// Make sure after move, source score is still greater than target score.
	shouldBalance := p.sourceScore > p.targetScore

	if !shouldBalance {
		log.Debug("skip balance "+p.kind.Resource.String(),
			zap.String("scheduler", scheduleName), zap.Uint64("region-id", p.region.GetID()), zap.Uint64("source-store", sourceID), zap.Uint64("target-store", targetID),
			zap.Int64("source-size", p.source.GetRegionSize()), zap.Float64("source-score", p.sourceScore),
			zap.Int64("source-influence", sourceInfluence),
			zap.Int64("target-size", p.target.GetRegionSize()), zap.Float64("target-score", p.targetScore),
			zap.Int64("target-influence", targetInfluence),
			zap.Int64("average-region-size", p.cluster.GetAverageRegionSize()),
			zap.Int64("tolerant-resource", tolerantResource))
	}
	return shouldBalance
}

func (p *balancePlan) getTolerantResource() int64 {
	if p.kind.Resource == core.LeaderKind && p.kind.Policy == core.ByCount {
		return int64(p.tolerantSizeRatio)
	}
	regionSize := p.region.GetApproximateSize()
	if regionSize < p.cluster.GetAverageRegionSize() {
		regionSize = p.cluster.GetAverageRegionSize()
	}
	return int64(float64(regionSize) * p.tolerantSizeRatio)
}

func adjustTolerantRatio(cluster opt.Cluster, kind core.ScheduleKind) float64 {
	tolerantSizeRatio := cluster.GetOpts().GetTolerantSizeRatio()
	if kind.Resource == core.LeaderKind && kind.Policy == core.ByCount {
		if tolerantSizeRatio == 0 {
			return leaderTolerantSizeRatio
		}
		return tolerantSizeRatio
	}

	if tolerantSizeRatio == 0 {
		var maxRegionCount float64
		stores := cluster.GetStores()
		for _, store := range stores {
			regionCount := float64(cluster.GetStoreRegionCount(store.GetID()))
			if maxRegionCount < regionCount {
				maxRegionCount = regionCount
			}
		}
		tolerantSizeRatio = maxRegionCount * adjustRatio
		if tolerantSizeRatio < minTolerantSizeRatio {
			tolerantSizeRatio = minTolerantSizeRatio
		}
	}
	return tolerantSizeRatio
}

func adjustBalanceLimit(cluster opt.Cluster, kind core.ResourceKind) uint64 {
	stores := cluster.GetStores()
	counts := make([]float64, 0, len(stores))
	for _, s := range stores {
		if s.IsUp() {
			counts = append(counts, float64(s.ResourceCount(kind)))
		}
	}
	limit, _ := stats.StandardDeviation(counts)
	return typeutil.MaxUint64(1, uint64(limit))
}

func getKeyRanges(args []string) ([]core.KeyRange, error) {
	var ranges []core.KeyRange
	for len(args) > 1 {
		startKey, err := url.QueryUnescape(args[0])
		if err != nil {
			return nil, errs.ErrQueryUnescape.Wrap(err).FastGenWithCause()
		}
		endKey, err := url.QueryUnescape(args[1])
		if err != nil {
			return nil, errs.ErrQueryUnescape.Wrap(err).FastGenWithCause()
		}
		args = args[2:]
		ranges = append(ranges, core.NewKeyRange(startKey, endKey))
	}
	if len(ranges) == 0 {
		return []core.KeyRange{core.NewKeyRange("", "")}, nil
	}
	return ranges, nil
}

// Influence records operator influence.
type Influence struct {
	Loads []float64
	Count float64
}

func (lhs *Influence) add(rhs *Influence, w float64) *Influence {
	var infl Influence
	for i := range lhs.Loads {
		infl.Loads = append(infl.Loads, lhs.Loads[i]+rhs.Loads[i]*w)
	}
	infl.Count = infl.Count + rhs.Count*w
	return &infl
}

// TODO: merge it into OperatorInfluence.
type pendingInfluence struct {
	op       *operator.Operator
	from, to uint64
	origin   Influence
}

func newPendingInfluence(op *operator.Operator, from, to uint64, infl Influence) *pendingInfluence {
	return &pendingInfluence{
		op:     op,
		from:   from,
		to:     to,
		origin: infl,
	}
}

// summaryPendingInfluence calculate the summary pending Influence for each store and return storeID -> Influence
// It makes each key/byte rate or count become (1+w) times to the origin value while f is the function to provide w(weight)
func summaryPendingInfluence(pendings map[*pendingInfluence]struct{}, f func(*operator.Operator) float64) map[uint64]*Influence {
	ret := make(map[uint64]*Influence)
	for p := range pendings {
		w := f(p.op)
		if w == 0 {
			delete(pendings, p)
		}
		if _, ok := ret[p.to]; !ok {
			ret[p.to] = &Influence{Loads: make([]float64, len(p.origin.Loads))}
		}
		ret[p.to] = ret[p.to].add(&p.origin, w)
		if _, ok := ret[p.from]; !ok {
			ret[p.from] = &Influence{Loads: make([]float64, len(p.origin.Loads))}
		}
		ret[p.from] = ret[p.from].add(&p.origin, -w)
	}
	return ret
}

type storeLoad struct {
	Loads []float64
	Count float64
}

func (load storeLoad) ToLoadPred(rwTy rwType, infl *Influence) *storeLoadPred {
	future := storeLoad{
		Loads: append(load.Loads[:0:0], load.Loads...),
		Count: load.Count,
	}
	if infl != nil {
		switch rwTy {
		case read:
			future.Loads[statistics.ByteDim] += infl.Loads[statistics.RegionReadBytes]
			future.Loads[statistics.KeyDim] += infl.Loads[statistics.RegionReadKeys]
		case write:
			future.Loads[statistics.ByteDim] += infl.Loads[statistics.RegionWriteBytes]
			future.Loads[statistics.KeyDim] += infl.Loads[statistics.RegionWriteKeys]
		}
		future.Count += infl.Count
	}
	return &storeLoadPred{
		Current: load,
		Future:  future,
	}
}

func stLdByteRate(ld *storeLoad) float64 {
	return ld.Loads[statistics.ByteDim]
}

func stLdKeyRate(ld *storeLoad) float64 {
	return ld.Loads[statistics.KeyDim]
}

func stLdCount(ld *storeLoad) float64 {
	return ld.Count
}

type storeLoadCmp func(ld1, ld2 *storeLoad) int

func negLoadCmp(cmp storeLoadCmp) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		return -cmp(ld1, ld2)
	}
}

func sliceLoadCmp(cmps ...storeLoadCmp) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		for _, cmp := range cmps {
			if r := cmp(ld1, ld2); r != 0 {
				return r
			}
		}
		return 0
	}
}

func stLdRankCmp(dim func(ld *storeLoad) float64, rank func(value float64) int64) storeLoadCmp {
	return func(ld1, ld2 *storeLoad) int {
		return rankCmp(dim(ld1), dim(ld2), rank)
	}
}

func rankCmp(a, b float64, rank func(value float64) int64) int {
	aRk, bRk := rank(a), rank(b)
	if aRk < bRk {
		return -1
	} else if aRk > bRk {
		return 1
	}
	return 0
}

// store load prediction
type storeLoadPred struct {
	Current storeLoad
	Future  storeLoad
	Expect  storeLoad
}

func (lp *storeLoadPred) min() *storeLoad {
	return minLoad(&lp.Current, &lp.Future)
}

func (lp *storeLoadPred) max() *storeLoad {
	return maxLoad(&lp.Current, &lp.Future)
}

func (lp *storeLoadPred) diff() *storeLoad {
	mx, mn := lp.max(), lp.min()
	loads := make([]float64, len(mx.Loads))
	for i := range loads {
		loads[i] = mx.Loads[i] - mn.Loads[i]
	}
	return &storeLoad{
		Loads: loads,
		Count: mx.Count - mn.Count,
	}
}

type storeLPCmp func(lp1, lp2 *storeLoadPred) int

func sliceLPCmp(cmps ...storeLPCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		for _, cmp := range cmps {
			if r := cmp(lp1, lp2); r != 0 {
				return r
			}
		}
		return 0
	}
}

func minLPCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.min(), lp2.min())
	}
}

func maxLPCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.max(), lp2.max())
	}
}

func diffCmp(ldCmp storeLoadCmp) storeLPCmp {
	return func(lp1, lp2 *storeLoadPred) int {
		return ldCmp(lp1.diff(), lp2.diff())
	}
}

func minLoad(a, b *storeLoad) *storeLoad {
	loads := make([]float64, len(a.Loads))
	for i := range loads {
		loads[i] = math.Min(a.Loads[i], b.Loads[i])
	}
	return &storeLoad{
		Loads: loads,
		Count: math.Min(a.Count, b.Count),
	}
}

func maxLoad(a, b *storeLoad) *storeLoad {
	loads := make([]float64, len(a.Loads))
	for i := range loads {
		loads[i] = math.Max(a.Loads[i], b.Loads[i])
	}
	return &storeLoad{
		Loads: loads,
		Count: math.Max(a.Count, b.Count),
	}
}

type storeLoadDetail struct {
	Store    *core.StoreInfo
	LoadPred *storeLoadPred
	HotPeers []*statistics.HotPeerStat
}

func (li *storeLoadDetail) toHotPeersStat() *statistics.HotPeersStat {
	totalLoads := make([]float64, statistics.RegionStatCount)
	if len(li.HotPeers) == 0 {
		return &statistics.HotPeersStat{
			TotalLoads:     totalLoads,
			TotalBytesRate: 0.0,
			TotalKeysRate:  0.0,
			TotalQueryRate: 0.0,
			Count:          0,
			Stats:          make([]statistics.HotPeerStatShow, 0),
		}
	}
	kind := write
	if li.HotPeers[0].Kind == statistics.ReadFlow {
		kind = read
	}

	peers := make([]statistics.HotPeerStatShow, 0, len(li.HotPeers))
	for _, peer := range li.HotPeers {
		if peer.HotDegree > 0 {
			peers = append(peers, toHotPeerStatShow(peer, kind))
			for i := range totalLoads {
				totalLoads[i] += peer.GetLoad(statistics.RegionStatKind(i))
			}
		}
	}

	b, k, q := getRegionStatKind(kind, statistics.ByteDim), getRegionStatKind(kind, statistics.KeyDim), getRegionStatKind(kind, statistics.QueryDim)
	byteRate := totalLoads[b]
	keyRate := totalLoads[k]
	queryRate := totalLoads[q]

	return &statistics.HotPeersStat{
		TotalLoads:     totalLoads,
		TotalBytesRate: byteRate,
		TotalKeysRate:  keyRate,
		TotalQueryRate: queryRate,
		Count:          len(peers),
		Stats:          peers,
	}
}

func toHotPeerStatShow(p *statistics.HotPeerStat, kind rwType) statistics.HotPeerStatShow {
	b, k, q := getRegionStatKind(kind, statistics.ByteDim), getRegionStatKind(kind, statistics.KeyDim), getRegionStatKind(kind, statistics.QueryDim)
	byteRate := p.Loads[b]
	keyRate := p.Loads[k]
	queryRate := p.Loads[q]
	return statistics.HotPeerStatShow{
		StoreID:        p.StoreID,
		RegionID:       p.RegionID,
		HotDegree:      p.HotDegree,
		ByteRate:       byteRate,
		KeyRate:        keyRate,
		QueryRate:      queryRate,
		AntiCount:      p.AntiCount,
		LastUpdateTime: p.LastUpdateTime,
	}
}
