package backtest

import (
	"strings"
	"testing"
)

func TestFormatting(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{commas(0), "0"},
		{commas(999), "999"},
		{commas(1000), "1,000"},
		{commas(1234567), "1,234,567"},
		{commas(-1234567), "-1,234,567"},
		{count(1, "payment", "payments"), "1 payment"},
		{count(0, "payment", "payments"), "0 payments"},
		{count(1204, "payment", "payments"), "1,204 payments"},
		{dollars(0), "$0"},
		{dollars(25), "$25"},
		{dollars(4.99), "$4.99"},
		{dollars(50.5), "$50.50"},
		{dollars(99.999), "$100"},
		{dollars(182347.4), "$182,347"},
		{dollars(-12.25), "-$12.25"},
		{percent(0, 10), "0%"},
		{percent(1, 3), "33%"},
		{percent(1, 2000), "under 0.1%"},
		{percent(3, 1000), "0.3%"},
		{percent(999, 1000), "over 99%"},
		{percent(10, 10), "100%"},
		{percent(1, 0), "n/a"},
		{days(30 * 86400), "30 days"},
		{days(86400), "1 day"},
		{days(86400*7 + 3600), "7 days"},
		{date(DefaultEpoch, 86400), "December 2, 2017"},
		{capitalize("payments"), "Payments"},
		{capitalize(""), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestSummaryVoice(t *testing.T) {
	base := func(action string) Report {
		return Report{Action: action, Epoch: DefaultEpoch, FromDT: 86400, ToDT: 31 * 86400,
			Period: Outcome{Fraud: Counts{10, 5000}}}
	}
	for _, tc := range []struct {
		name string
		r    func() Report
		want []string
		not  []string
	}{
		{"one fraud", func() Report {
			r := base("block")
			r.Changed = Outcome{All: Counts{1, 300}, Fraud: Counts{1, 300}}
			r.Matched = r.Changed
			return r
		}, []string{"blocked 1 payment worth $300.", "It was later disputed as fraud, which is 6% of all fraud dollars"}, []string{"legitimate"}},
		{"all legit", func() Report {
			r := base("review")
			r.Changed = Outcome{All: Counts{5, 600}, Legit: Counts{5, 600}}
			r.Matched = r.Changed
			return r
		}, []string{"sent 5 payments worth $600 to review.", "None of them were later disputed as fraud."}, []string{"0%", "also"}},
		{"allow partly blocked", func() Report {
			r := base("allow")
			r.Changed = Outcome{All: Counts{4, 400}, Fraud: Counts{1, 100}, Legit: Counts{3, 300}}
			r.Matched = r.Changed
			r.OverridesBlock = &Outcome{All: Counts{3, 300}}
			return r
		}, []string{"let through 4 payments worth $400", "1 of them (25%), worth $100, was later disputed", "3 of them are blocked today and the rest go to review."}, nil},
		{"label time", func() Report {
			r := base("block")
			r.UseLabelTime = true
			r.AsOfDT = 40 * 86400
			r.Changed = Outcome{All: Counts{2, 200}, Fraud: Counts{1, 100}, Legit: Counts{1, 100}}
			r.Matched = r.Changed
			return r
		}, []string{"50% of them were disputed as fraud by January 10, 2018", "SIMULATED"}, nil},
		{"many immature", func() Report {
			r := base("block")
			r.Immature = Counts{12, 50}
			r.MaturityDays = 45
			return r
		}, []string{"12 payments from the last 45 days were excluded because their disputes may not have arrived yet."}, nil},
	} {
		r := tc.r()
		got := r.Summary()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
		for _, w := range tc.not {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q contains %q", tc.name, got, w)
			}
		}
	}
}
