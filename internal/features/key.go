package features

import (
	"cmp"
	"fmt"
	"math"
	"strconv"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Entity is who a velocity feature is about.
type Entity uint8

// The entities, in schema.Entities order.
const (
	// Card is card1, the anonymized card identifier. In IEEE-CIS it is
	// coarse: many customers share a card1 value.
	Card Entity = iota
	// UID approximates one customer: card1, addr1, and the day the card was
	// first used, recovered as floor(TransactionDT / 86400) - D1, where D1 is
	// days since that first use. This is the identity rebuilt by the
	// IEEE-CIS competition's first-place team, Chris Deotte and Konstantin
	// Yakovlev, in their solution write-up (Kaggle competition discussion,
	// 2019). It is an approximation: two customers can share it, and one
	// customer can split across it when D1 drifts or is clipped.
	UID
	// Device is the identity table's DeviceInfo string.
	Device
	// Email is the purchaser email domain. The dataset has only the domain,
	// never the address, so this is a domain-level signal ("payments from
	// anonymous.com in the last hour"), not a per-person one.
	Email

	NumEntities = 4
)

var entityNames = [NumEntities]string{"card", "uid", "device", "email"}

func (e Entity) String() string {
	if int(e) < len(entityNames) {
		return entityNames[e]
	}
	return "Entity(" + strconv.Itoa(int(e)) + ")"
}

// tracksDistinct reports whether the entity has a distinct-cards feature.
// Card testing shows up as many cards on one device, or one email domain.
func (e Entity) tracksDistinct() bool { return e == Device || e == Email }

func init() {
	if len(schema.Entities) != NumEntities {
		panic("features: schema.Entities changed; update the Entity constants")
	}
	for i, name := range schema.Entities {
		if entityNames[i] != name {
			panic("features: entity " + strconv.Itoa(i) + " is " + name + " in schema, " + entityNames[i] + " here")
		}
	}
}

// Key names one entity instance. Numeric parts hold float64 bit patterns so
// that Key is comparable, usable as a map key, and hashable without
// allocating; a string part holds DeviceInfo or the email domain.
type Key struct {
	Entity Entity
	Num    [3]uint64
	Str    string
}

// floatBits normalizes -0 to +0 so equal numbers make equal keys.
func floatBits(x float64) uint64 { return math.Float64bits(x + 0) }

func present(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// CardKey is the card entity for card1.
func CardKey(card1 float64) (Key, bool) {
	if !present(card1) {
		return Key{}, false
	}
	return Key{Entity: Card, Num: [3]uint64{floatBits(card1)}}, true
}

// UIDKey is the approximate customer: card1, addr1, and the card's first-use
// day, floor(dt / 86400) - d1. It is missing if any part is.
func UIDKey(card1, addr1 float64, dt int64, d1 float64) (Key, bool) {
	if !present(card1) || !present(addr1) || !present(d1) {
		return Key{}, false
	}
	day := float64(floorDiv(dt, data.SecondsPerDay))
	return Key{Entity: UID, Num: [3]uint64{floatBits(card1), floatBits(addr1), floatBits(day - d1)}}, true
}

// DeviceKey is the device entity for a DeviceInfo string.
func DeviceKey(deviceInfo string) (Key, bool) {
	return Key{Entity: Device, Str: deviceInfo}, deviceInfo != ""
}

// EmailKey is the email entity for a purchaser email domain.
func EmailKey(domain string) (Key, bool) {
	return Key{Entity: Email, Str: domain}, domain != ""
}

// KeysOf returns t's key for every entity, with ok[e] false where the key is
// missing. A transaction with a missing key gets missing features for that
// entity rather than being pooled with every other missing-key transaction.
func KeysOf(t *data.Txn) (keys [NumEntities]Key, ok [NumEntities]bool) {
	keys[Card], ok[Card] = CardKey(t.Card1)
	keys[UID], ok[UID] = UIDKey(t.Card1, t.Addr1, t.DT, t.D1)
	keys[Device], ok[Device] = DeviceKey(t.DeviceInfo)
	keys[Email], ok[Email] = EmailKey(t.PEmail)
	return keys, ok
}

// Hash is a deterministic 64-bit hash of the key: FNV-1a over its parts,
// then a finalizer so the low bits mix well. It must not use a per-process
// seed (hash/maphash does), because sketch cells and shard assignments are
// written into snapshots and must mean the same thing after a restart.
func (k Key) Hash() uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	h = (h ^ uint64(k.Entity)) * prime
	for _, n := range k.Num {
		for i := 0; i < 64; i += 8 {
			h = (h ^ (n >> i & 0xff)) * prime
		}
	}
	for i := 0; i < len(k.Str); i++ {
		h = (h ^ uint64(k.Str[i])) * prime
	}
	return mix64(h)
}

// mix64 is the splitmix64 finalizer.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func (k Key) String() string {
	switch k.Entity {
	case Card:
		return fmt.Sprintf("card:%g", math.Float64frombits(k.Num[0]))
	case UID:
		return fmt.Sprintf("uid:%g/%g/%g", math.Float64frombits(k.Num[0]),
			math.Float64frombits(k.Num[1]), math.Float64frombits(k.Num[2]))
	}
	return k.Entity.String() + ":" + strconv.Quote(k.Str)
}

// compareKeys is a total order on keys, used to write snapshots in a
// deterministic order.
func compareKeys(a, b Key) int {
	if c := cmp.Compare(a.Entity, b.Entity); c != 0 {
		return c
	}
	for i := range a.Num {
		if c := cmp.Compare(a.Num[i], b.Num[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(a.Str, b.Str)
}

// floorDiv divides rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
