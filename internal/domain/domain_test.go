package domain

import (
	"errors"
	"testing"
)

func validRule() Rule {
	return Rule{
		PublisherID: "acme-tv",
		Segment:     Segment{Device: "CTV", Geo: "us", Genre: "", DemandPartner: "magnite"},
		FloorMicros: 4_500_000,
		Status:      RuleActive,
	}
}

func TestNormalizeAndValidate_Normalizes(t *testing.T) {
	r := validRule()
	if err := NormalizeAndValidate(&r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Segment{Device: "ctv", Geo: "US", Genre: Any, DemandPartner: "magnite"}
	if r.Segment != want {
		t.Fatalf("segment = %+v, want %+v", r.Segment, want)
	}
	if r.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", r.Currency)
	}
}

func TestNormalizeAndValidate_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Rule)
		field string
	}{
		{"bad publisher", func(r *Rule) { r.PublisherID = "Acme TV" }, "publisher_id"},
		{"empty publisher", func(r *Rule) { r.PublisherID = "" }, "publisher_id"},
		{"bad device", func(r *Rule) { r.Segment.Device = "fridge" }, "segment.device"},
		{"bad geo", func(r *Rule) { r.Segment.Geo = "USA" }, "segment.geo"},
		{"bad genre", func(r *Rule) { r.Segment.Genre = "drama; drop table" }, "segment.genre"},
		{"bad partner", func(r *Rule) { r.Segment.DemandPartner = "../etc" }, "segment.demand_partner"},
		{"floor too low", func(r *Rule) { r.FloorMicros = 9_999 }, "floor_micros"},
		{"floor too high", func(r *Rule) { r.FloorMicros = MaxFloorMicros + 1 }, "floor_micros"},
		{"negative floor", func(r *Rule) { r.FloorMicros = -1 }, "floor_micros"},
		{"currency", func(r *Rule) { r.Currency = "EUR" }, "currency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRule()
			tc.mut(&r)
			err := NormalizeAndValidate(&r)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T", err)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("want field %q flagged, got %v", tc.field, ve.Fields)
			}
		})
	}
}

func TestNormalizeAndValidate_ReportsAllFields(t *testing.T) {
	r := Rule{PublisherID: "BAD", Segment: Segment{Device: "x", Geo: "zz1"}, FloorMicros: 0, Currency: "GBP"}
	err := NormalizeAndValidate(&r)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if len(ve.Fields) != 5 {
		t.Fatalf("want 5 field errors, got %d: %v", len(ve.Fields), ve.Fields)
	}
}

func seg(device, geo string) Segment {
	return Segment{Device: device, Geo: geo, Genre: Any, DemandPartner: Any}
}

func rule(s Segment, micros int64) Rule {
	return Rule{PublisherID: "p", Segment: s, FloorMicros: micros, Status: RuleActive}
}

func TestComputeOps_CreateUpdateDeleteAndUnmanaged(t *testing.T) {
	rules := []Rule{
		rule(seg("ctv", "US"), 5_000_000),     // update 4.0 -> 5.0 (25%)
		rule(seg("mobile", "US"), 1_000_000),  // create
		rule(seg("desktop", "GB"), 2_000_000), // unchanged
		{PublisherID: "p", Segment: seg("tablet", "US"), FloorMicros: 1_000_000, Status: RuleDisabled},
	}
	platform := []PlatformFloor{
		{Segment: seg("ctv", "US"), FloorMicros: 4_000_000, ManagedBy: ManagedBy},
		{Segment: seg("desktop", "GB"), FloorMicros: 2_000_000, ManagedBy: ManagedBy},
		{Segment: seg("tablet", "US"), FloorMicros: 1_000_000, ManagedBy: ManagedBy}, // disabled rule -> delete
		{Segment: seg("ctv", "CA"), FloorMicros: 3_000_000, ManagedBy: "human"},      // not ours -> keep
	}
	ops, err := ComputeOps(rules, platform, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("want 3 ops, got %d: %+v", len(ops), ops)
	}
	if ops[0].Kind != OpDelete || ops[0].Segment != seg("tablet", "US") {
		t.Errorf("op0 = %+v, want delete tablet/US", ops[0])
	}
	if ops[1].Kind != OpUpdate || ops[1].FromMicros != 4_000_000 || ops[1].ToMicros != 5_000_000 || ops[1].Risky {
		t.Errorf("op1 = %+v, want non-risky update 4.0 -> 5.0", ops[1])
	}
	if ops[2].Kind != OpCreate || ops[2].Segment != seg("mobile", "US") {
		t.Errorf("op2 = %+v, want create mobile/US", ops[2])
	}
}

func TestComputeOps_RiskyGuardrails(t *testing.T) {
	platform := []PlatformFloor{
		{Segment: seg("ctv", "US"), FloorMicros: 5_000_000, ManagedBy: ManagedBy},
		{Segment: seg("ctv", "CA"), FloorMicros: 5_000_000, ManagedBy: "human"},
	}
	rules := []Rule{
		rule(seg("ctv", "US"), 50_000_000), // fat-finger: $5 -> $50, +900%
		rule(seg("ctv", "CA"), 5_000_000),  // same value but takes over a human floor
	}
	ops, err := ComputeOps(rules, platform, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 ops, got %+v", ops)
	}
	for _, op := range ops {
		if !op.Risky || op.RiskyReason == "" {
			t.Errorf("want risky op with reason, got %+v", op)
		}
	}
	p := Plan{Ops: ops}
	if !p.HasRisky() {
		t.Error("HasRisky = false, want true")
	}
}

func TestComputeOps_DecreaseOverGuardrailIsRisky(t *testing.T) {
	platform := []PlatformFloor{{Segment: seg("ctv", "US"), FloorMicros: 10_000_000, ManagedBy: ManagedBy}}
	ops, _ := ComputeOps([]Rule{rule(seg("ctv", "US"), 4_000_000)}, platform, DefaultLimits)
	if len(ops) != 1 || !ops[0].Risky {
		t.Fatalf("a 60%% cut should be risky, got %+v", ops)
	}
}

func TestComputeOps_MaxOps(t *testing.T) {
	var rules []Rule
	for _, g := range []string{"US", "CA", "GB", "DE"} {
		rules = append(rules, rule(seg("ctv", g), 1_000_000))
	}
	_, err := ComputeOps(rules, nil, PlanLimits{MaxOps: 3, RiskyChangePct: 50})
	if !errors.Is(err, ErrPlanTooLarge) {
		t.Fatalf("want ErrPlanTooLarge, got %v", err)
	}
}

func TestComputeOps_Deterministic(t *testing.T) {
	var rules []Rule
	for _, g := range []string{"US", "CA", "GB", "DE", "FR", "JP"} {
		rules = append(rules, rule(seg("ctv", g), 1_000_000))
	}
	first, _ := ComputeOps(rules, nil, DefaultLimits)
	for range 20 {
		again, _ := ComputeOps(rules, nil, DefaultLimits)
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("non-deterministic order at %d: %+v vs %+v", i, first[i], again[i])
			}
		}
	}
}

func TestFingerprint_OrderIndependentAndSensitive(t *testing.T) {
	a := []PlatformFloor{
		{Segment: seg("ctv", "US"), FloorMicros: 1, ManagedBy: ManagedBy},
		{Segment: seg("ctv", "CA"), FloorMicros: 2, ManagedBy: ManagedBy},
	}
	b := []PlatformFloor{a[1], a[0]}
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("fingerprint depends on order")
	}
	c := []PlatformFloor{a[0], {Segment: seg("ctv", "CA"), FloorMicros: 3, ManagedBy: ManagedBy}}
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("fingerprint ignores a floor change")
	}
}

func TestChangePct(t *testing.T) {
	cases := []struct{ from, to, want int64 }{
		{100, 150, 50}, {100, 151, 51}, {100, 40, 60}, {0, 10, 100}, {100, 100, 0},
	}
	for _, c := range cases {
		if got := changePct(c.from, c.to); got != c.want {
			t.Errorf("changePct(%d,%d) = %d, want %d", c.from, c.to, got, c.want)
		}
	}
}
