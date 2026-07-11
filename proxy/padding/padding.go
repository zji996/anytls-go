package padding

import (
	"anytls/util"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	randv2 "math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/sagernet/sing/common/atomic"
)

const (
	CheckMark      = -1
	MaxPaddingSize = 1<<16 - 1
)

var defaultPaddingScheme = []byte(`stop=8
0=30-30
1=100-400
2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000
3=9-9,500-1000
4=500-1000
5=500-1000
6=500-1000
7=500-1000`)

type PaddingFactory struct {
	scheme    util.StringMap
	rules     map[uint32][]paddingRule
	fixed     map[uint32][]int
	RawScheme []byte
	Stop      uint32
	Md5       string
}

type paddingRule struct {
	check bool
	min   int
	max   int
}

var DefaultPaddingFactory atomic.TypedValue[*PaddingFactory]

func init() {
	UpdatePaddingScheme(defaultPaddingScheme)
}

func NewDefaultPaddingFactory() *atomic.TypedValue[*PaddingFactory] {
	factory := &atomic.TypedValue[*PaddingFactory]{}
	factory.Store(DefaultPaddingFactory.Load())
	return factory
}

func UpdatePaddingScheme(rawScheme []byte) bool {
	return UpdatePaddingFactory(&DefaultPaddingFactory, rawScheme)
}

func UpdatePaddingFactory(factory *atomic.TypedValue[*PaddingFactory], rawScheme []byte) bool {
	if p := NewPaddingFactory(rawScheme); p != nil {
		factory.Store(p)
		return true
	}
	return false
}

func NewPaddingFactory(rawScheme []byte) *PaddingFactory {
	rawScheme = slices.Clone(rawScheme)
	p := &PaddingFactory{
		RawScheme: rawScheme,
		Md5:       fmt.Sprintf("%x", md5.Sum(rawScheme)),
	}
	scheme := util.StringMapFromBytes(rawScheme)
	if len(scheme) == 0 {
		return nil
	}
	if stop, err := strconv.ParseUint(scheme["stop"], 10, 32); err == nil {
		p.Stop = uint32(stop)
	} else {
		return nil
	}
	p.scheme = scheme
	var ok bool
	p.rules, ok = compileRules(scheme)
	if !ok {
		return nil
	}
	if authRules := p.rules[0]; len(authRules) > 0 && (len(authRules) != 1 || authRules[0].check) {
		return nil
	}
	p.fixed = compileFixedSizes(p.rules)
	return p
}

func (p *PaddingFactory) GenerateRecordPayloadSizes(pkt uint32) (pktSizes []int) {
	return p.generateRecordPayloadSizes(pkt, nil, nil)
}

func (p *PaddingFactory) GenerateRecordPayloadSizesWithRNG(pkt uint32, rng *randv2.ChaCha8) []int {
	return p.generateRecordPayloadSizes(pkt, rng, nil)
}

func (p *PaddingFactory) GenerateRecordPayloadSizesWithRNGInto(pkt uint32, rng *randv2.ChaCha8, destination []int) []int {
	return p.generateRecordPayloadSizes(pkt, rng, destination)
}

func (p *PaddingFactory) generateRecordPayloadSizes(pkt uint32, rng *randv2.ChaCha8, destination []int) (pktSizes []int) {
	rules := p.rules[pkt]
	if len(rules) == 0 {
		return nil
	}
	if fixed, ok := p.fixed[pkt]; ok {
		return fixed
	}
	if cap(destination) >= len(rules) {
		pktSizes = destination[:0]
	} else {
		pktSizes = make([]int, 0, len(rules))
	}
	for _, rule := range rules {
		if rule.check {
			pktSizes = append(pktSizes, CheckMark)
		} else if rule.min == rule.max {
			pktSizes = append(pktSizes, rule.min)
		} else if rng != nil {
			pktSizes = append(pktSizes, randomIntFromUint64(rule.min, rule.max, rng.Uint64()))
		} else {
			pktSizes = append(pktSizes, randomInt(rule.min, rule.max))
		}
	}
	return
}

func compileRules(scheme util.StringMap) (map[uint32][]paddingRule, bool) {
	rules := make(map[uint32][]paddingRule)
	for key, value := range scheme {
		if key == "stop" {
			continue
		}
		pkt, err := strconv.ParseUint(key, 10, 32)
		if err != nil {
			return nil, false
		}
		for _, rawRule := range strings.Split(value, ",") {
			if rawRule == "c" {
				rules[uint32(pkt)] = append(rules[uint32(pkt)], paddingRule{check: true})
				continue
			}
			minValue, maxValue, ok := parseRange(rawRule)
			if !ok {
				return nil, false
			}
			rules[uint32(pkt)] = append(rules[uint32(pkt)], paddingRule{min: minValue, max: maxValue})
		}
	}
	return rules, true
}

func compileFixedSizes(rules map[uint32][]paddingRule) map[uint32][]int {
	fixed := make(map[uint32][]int)
	for pkt, pktRules := range rules {
		sizes := make([]int, 0, len(pktRules))
		allFixed := true
		for _, rule := range pktRules {
			switch {
			case rule.check:
				sizes = append(sizes, CheckMark)
			case rule.min == rule.max:
				sizes = append(sizes, rule.min)
			default:
				allFixed = false
			}
		}
		if allFixed {
			fixed[pkt] = sizes
		}
	}
	return fixed
}

func parseRange(raw string) (int, int, bool) {
	minRaw, maxRaw, ok := strings.Cut(raw, "-")
	if !ok {
		return 0, 0, false
	}
	minValue64, err := strconv.ParseInt(minRaw, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	maxValue64, err := strconv.ParseInt(maxRaw, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	minValue64, maxValue64 = min(minValue64, maxValue64), max(minValue64, maxValue64)
	if minValue64 <= 0 || maxValue64 <= 0 || maxValue64 > MaxPaddingSize {
		return 0, 0, false
	}
	return int(minValue64), int(maxValue64), true
}

func randomInt(minValue int, maxValue int) int {
	delta := maxValue - minValue
	if delta <= 0 {
		return minValue
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return minValue
	}
	return randomIntFromUint64(minValue, maxValue, binary.LittleEndian.Uint64(b[:]))
}

func randomIntFromUint64(minValue int, maxValue int, random uint64) int {
	delta := maxValue - minValue
	if delta <= 0 {
		return minValue
	}
	return minValue + int(random%uint64(delta))
}
