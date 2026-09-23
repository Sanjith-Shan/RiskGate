package loadgen

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestBody(t *testing.T) {
	line := `{"payment_id":"pay_\"q\"","created":1767225600,"amount":1999,"currency":"usd","risk_fields":{"note":"<café>"}}`
	req, err := ParseRequest([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(req.Body(7, false)); got != line {
		t.Fatalf("non-unique body changed:\n got %s\nwant %s", got, line)
	}

	var got, want map[string]any
	if err := json.Unmarshal(req.Body(42, true), &got); err != nil {
		t.Fatalf("rewritten body is not JSON: %v\n%s", err, req.Body(42, true))
	}
	if err := json.Unmarshal([]byte(line), &want); err != nil {
		t.Fatal(err)
	}
	if got["payment_id"] != `pay_"q"_lg42` {
		t.Fatalf("payment_id = %v", got["payment_id"])
	}
	want["payment_id"] = got["payment_id"]
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("other fields changed:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestRequestWithoutPaymentID(t *testing.T) {
	req, err := ParseRequest([]byte(`{"amount":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(req.Body(3, true), &got); err != nil {
		t.Fatal(err)
	}
	if got["payment_id"] != "_lg3" {
		t.Fatalf("payment_id = %v", got["payment_id"])
	}
}

func TestReadRequestsErrors(t *testing.T) {
	for name, input := range map[string]string{
		"empty":              "\n\n",
		"not json":           `{"payment_id":"a"}` + "\nnope\n",
		"array":              `[1,2]`,
		"numeric payment_id": `{"payment_id":5}`,
		"placeholder text":   `{"payment_id":"a","x":"__loadgen_payment_id_placeholder__"}`,
	} {
		if _, err := ReadRequests(strings.NewReader(input)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ReadRequests(strings.NewReader("\n" + `{"payment_id":"a"}` + "\nbad")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should name line 3: %v", err)
	}
}

func BenchmarkRequestBodyUnique(b *testing.B) {
	req, err := ParseRequest([]byte(`{"payment_id":"pay_1","created":1767225600,"amount":1999,"currency":"usd","risk_fields":{"card_network":"visa","device_type":"mobile"}}`))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	var i int64
	for b.Loop() {
		i++
		_ = req.Body(i, true)
	}
}

func TestMachineLabel(t *testing.T) {
	m := DetectMachine()
	if m.LogicalCPUs <= 0 || m.GOMAXPROCS <= 0 || m.GoVersion == "" || m.CPU == "" {
		t.Fatalf("incomplete label %+v", m)
	}
	s := m.String()
	if !strings.Contains(s, "GOMAXPROCS=") || !strings.Contains(s, m.GoVersion) {
		t.Fatalf("label %q", s)
	}
	t.Log(s)
}
