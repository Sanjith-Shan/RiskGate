package data

import "testing"

func TestCalendarMonthsAndSplits(t *testing.T) {
	c := Calendar{Origin: 86400}
	const d = SecondsPerDay
	tests := []struct {
		dt    int64
		month int
		split Split
	}{
		{0, 0, Train}, // before the origin clamps to month 0
		{86400, 0, Train},
		{86400 + 30*d - 1, 0, Train},
		{86400 + 30*d, 1, Train},
		{86400 + 120*d - 1, 3, Train},
		{86400 + 120*d, 4, Valid},
		{86400 + 150*d - 1, 4, Valid},
		{86400 + 150*d, 5, Test},
		{86400 + 180*d, 5, Test}, // the partial seventh month folds into test
		{15811131, 5, Test},      // IEEE-CIS's last TransactionDT
	}
	for _, tt := range tests {
		if got := c.Month(tt.dt); got != tt.month {
			t.Errorf("Month(%d) = %d, want %d", tt.dt, got, tt.month)
		}
		if got := c.Split(tt.dt); got != tt.split {
			t.Errorf("Split(%d) = %v, want %v", tt.dt, got, tt.split)
		}
	}
	if got := c.MonthStart(4); got != 86400+120*d {
		t.Errorf("MonthStart(4) = %d", got)
	}
}

func TestCalendarForAnchorsAtDayStart(t *testing.T) {
	txns := []Txn{{DT: 200000}, {DT: 90061}, {DT: 500000}}
	if got := CalendarFor(txns).Origin; got != 86400 {
		t.Errorf("origin: got %d, want 86400", got)
	}
	if got := CalendarFor(nil).Origin; got != 0 {
		t.Errorf("empty origin: got %d, want 0", got)
	}
}

func TestFixtureSplit(t *testing.T) {
	txns := loadFixture(t)
	c := CalendarFor(txns)
	count := map[Split]int{}
	for i := range txns {
		count[c.Split(txns[i].DT)]++
	}
	if count[Train] != 7 || count[Valid] != 1 || count[Test] != 2 {
		t.Errorf("got %v, want 7 train, 1 valid, 2 test", count)
	}
}

func TestSplitString(t *testing.T) {
	for s, want := range map[Split]string{Train: "train", Valid: "valid", Test: "test", Split(9): "Split(9)"} {
		if s.String() != want {
			t.Errorf("got %q, want %q", s.String(), want)
		}
	}
}

func TestFloorDiv(t *testing.T) {
	for _, tt := range []struct{ a, b, want int64 }{
		{7, 2, 3}, {-7, 2, -4}, {-8, 2, -4}, {0, 5, 0}, {-1, 86400, -1},
	} {
		if got := floorDiv(tt.a, tt.b); got != tt.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
