package main

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

func TestWriteReplayRoundTrips(t *testing.T) {
	txns := []data.Txn{
		{ID: 1, DT: 86400, Amount: 5.841, ProductCode: "C", Card1: 5740, Addr1: data.NaN, Addr2: data.NaN, Dist1: data.NaN, D1: 0, PEmail: "gmail.com", IsFraud: 1},
		{ID: 2, DT: 86400, Amount: 58.95, ProductCode: "W", Card1: 4141, Addr1: 441, Addr2: 87, Dist1: 9, D1: data.NaN, DeviceInfo: "SM-A520W Build/NRD90M", IsFraud: 0},
	}
	path := filepath.Join(t.TempDir(), "replay.jsonl")
	n, err := writeReplay(path, txns)
	if err != nil || n != 2 {
		t.Fatalf("writeReplay: %d, %v", n, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		if strings.Contains(sc.Text(), "is_fraud") {
			t.Fatalf("line %d carries the label: %s", i, sc.Text())
		}
		var ev data.ReplayEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		got, err := ev.Txn()
		if err != nil {
			t.Fatal(err)
		}
		want := txns[i]
		want.IsFraud = 0 // absent label decodes as 0 through ReplayEvent
		if !sameTxn(got, want) {
			t.Errorf("line %d: got %+v, want %+v", i, got, want)
		}
	}
}

func sameTxn(a, b data.Txn) bool {
	nums := func(t data.Txn) []float64 { return []float64{t.Amount, t.Card1, t.Addr1, t.Addr2, t.Dist1, t.D1} }
	na, nb := nums(a), nums(b)
	for i := range na {
		if !eq(na[i], nb[i]) {
			return false
		}
	}
	a.Amount, a.Card1, a.Addr1, a.Addr2, a.Dist1, a.D1 = 0, 0, 0, 0, 0, 0
	b.Amount, b.Card1, b.Addr1, b.Addr2, b.Dist1, b.D1 = 0, 0, 0, 0, 0, 0
	return a == b
}

func TestWriteReplayRejectsDisorder(t *testing.T) {
	txns := []data.Txn{{ID: 2, DT: 5}, {ID: 1, DT: 5}}
	if _, err := writeReplay(filepath.Join(t.TempDir(), "r.jsonl"), txns); err == nil {
		t.Fatal("out-of-order input accepted")
	}
}

func TestSendAllSequentialInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	inflight, maxInflight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		maxInflight = max(maxInflight, inflight)
		mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		inflight--
		mu.Unlock()
		if strings.Contains(string(b), "bad") {
			http.Error(w, "nope", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	n, _, err := sendAll(strings.NewReader("{\"a\":1}\n\n{\"a\":2}\n{\"a\":3}\n"), srv.URL, io.Discard)
	if err != nil || n != 3 {
		t.Fatalf("sendAll: %d, %v", n, err)
	}
	if strings.Join(got, "|") != `{"a":1}|{"a":2}|{"a":3}` || maxInflight != 1 {
		t.Fatalf("got %v with %d in flight", got, maxInflight)
	}
	if n, _, err := sendAll(strings.NewReader("{\"a\":1}\n{\"bad\":1}\n{\"a\":3}\n"), srv.URL, io.Discard); err == nil || n != 1 {
		t.Fatalf("non-200 not reported: %d, %v", n, err)
	}
}

func TestEq(t *testing.T) {
	if !eq(math.NaN(), math.NaN()) || eq(0, math.Copysign(0, -1)) || eq(1, math.Nextafter(1, 2)) || !eq(0.1, 0.1) {
		t.Fatal("eq")
	}
	if !math.IsInf(absDiff(1, math.NaN()), 1) || absDiff(math.NaN(), math.NaN()) != 0 || absDiff(1, 3) != 2 {
		t.Fatal("absDiff")
	}
}
