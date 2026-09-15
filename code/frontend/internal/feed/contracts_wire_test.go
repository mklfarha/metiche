package feed

import (
	"encoding/json"
	"testing"
)

// TestLiveContractsCarryFieldsIssuesAndContractKeys: the backend's fields,
// issues and a conflict's contract_key reach the board's model.
func TestLiveContractsCarryFieldsIssuesAndContractKeys(t *testing.T) {
	var w contractsWire
	raw := `{"contracts":[{"key":"POST /api/login","kind":"http_endpoint","agreement":"mismatch",
		"issues":[{"kind":"missing_out","path":"expires_at","expected":"timestamp","direction":"out","severity":"high","producer":"S-1","consumer":"S-2"}],
		"produces":[{"role":"produces","session_key":"S-1","shape_hash":"h1","field_count":1,"fields":[{"path":"token","type":"string","direction":"out","required":true}]}],
		"consumes":[{"role":"consumes","session_key":"S-2","shape_hash":"h2","field_count":2}]}]}`
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	cs := w.contracts()
	if len(cs) != 1 || !cs[0].ServerVerdict || len(cs[0].Issues) != 1 || cs[0].Issues[0].Consumer != "S-2" {
		t.Fatalf("contracts = %+v", cs)
	}
	if p := cs[0].Producers(); len(p) != 1 || len(p[0].Fields) != 1 || p[0].Fields[0].Name != "token" || !p[0].Fields[0].Required {
		t.Fatalf("producer fields = %+v", p)
	}

	var cj conflictJSON
	if err := json.Unmarshal([]byte(`{"key":"CF-3","kind":"contract_mismatch","contract_key":"POST /api/login"}`), &cj); err != nil {
		t.Fatal(err)
	}
	if c := cj.conflict(nil); c.ContractKey != "POST /api/login" {
		t.Fatalf("conflict contract key = %q", c.ContractKey)
	}
}
