package padding

import (
	"anytls/util"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/sagernet/sing/common/atomic"
)

const CheckMark = -1

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
	p := &PaddingFactory{
		RawScheme: rawScheme,
		Md5:       fmt.Sprintf("%x", md5.Sum(rawScheme)),
	}
	scheme := util.StringMapFromBytes(rawScheme)
	if len(scheme) == 0 {
		return nil
	}
	if stop, err := strconv.Atoi(scheme["stop"]); err == nil {
		p.Stop = uint32(stop)
	} else {
		return nil
	}
	p.scheme = scheme
	p.rules = compileRules(scheme)
	p.fixed = compileFixedSizes(p.rules)
	return p
}

func (p *PaddingFactory) GenerateRecordPayloadSizes(pkt uint32) (pktSizes []int) {
	rules := p.rules[pkt]
	if len(rules) == 0 {
		return nil
	}
	if fixed, ok := p.fixed[pkt]; ok {
		return fixed
	}
	pktSizes = make([]int, 0, len(rules))
	for _, rule := range rules {
		if rule.check {
			pktSizes = append(pktSizes, CheckMark)
		} else if rule.min == rule.max {
			pktSizes = append(pktSizes, rule.min)
		} else {
			pktSizes = append(pktSizes, randomInt(rule.min, rule.max))
		}
	}
	return
}

func compileRules(scheme util.StringMap) map[uint32][]paddingRule {
	rules := make(map[uint32][]paddingRule)
	for key, value := range scheme {
		pkt, err := strconv.ParseUint(key, 10, 32)
		if err != nil {
			continue
		}
		for _, rawRule := range strings.Split(value, ",") {
			if rawRule == "c" {
				rules[uint32(pkt)] = append(rules[uint32(pkt)], paddingRule{check: true})
				continue
			}
			minValue, maxValue, ok := parseRange(rawRule)
			if !ok {
				continue
			}
			rules[uint32(pkt)] = append(rules[uint32(pkt)], paddingRule{min: minValue, max: maxValue})
		}
	}
	return rules
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
	if minValue64 <= 0 || maxValue64 <= 0 {
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
	return minValue + int(binary.LittleEndian.Uint64(b[:])%uint64(delta))
}
