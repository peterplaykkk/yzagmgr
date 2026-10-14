package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// UC-114: a new profile may declare its selected model independently from the
// policy default. The models inventory, when supplied, is the authority for
// validating that selection; old name-only callers remain supported.
func TestProfileCreateSelectedModelAndInventory(t *testing.T) {
	srv := newContractServer(t)
	const tool = string(family.Claude)

	legacy := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"Legacy create"}`, true)
	if !legacy.ok {
		t.Fatalf("name-only create must remain compatible: %s", legacy.body)
	}

	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"Selected second","modelId":"second","models":["second"]}`, true)
	if !created.ok {
		t.Fatalf("modelId in the supplied models inventory should be accepted: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil {
		t.Fatalf("create response is not a profile: %v", err)
	}
	if card.ModelID != "second" || len(card.Models) != 1 || card.Models[0] != "second" {
		t.Fatalf("create readback lost the explicit single-model selection: modelId=%q models=%v", card.ModelID, card.Models)
	}

	listed := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/profiles", "", true)
	if !listed.ok {
		t.Fatalf("profile list readback failed: %s", listed.body)
	}
	var page profileListBody
	if err := json.Unmarshal(listed.data, &page); err != nil {
		t.Fatalf("profile list is not a profile page: %v", err)
	}
	found := false
	for _, item := range page.Items {
		if item.ID == card.ID {
			found = true
			if item.ModelID != "second" || len(item.Models) != 1 || item.Models[0] != "second" {
				t.Errorf("list readback substituted policy default for the selected model: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("created profile %q missing from list", card.ID)
	}

	before := persistedProfiles(t, srv)
	emptyModelID := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"Blank selected model","modelId":" ","models":[]}`, true)
	if emptyModelID.ok || emptyModelID.code != apierr.CodeInvalidRequest {
		t.Fatalf("explicit blank modelId must be invalid_request while omission remains compatible: %s", emptyModelID.body)
	}
	if after := persistedProfiles(t, srv); string(after) != string(before) {
		t.Fatalf("blank modelId rejection changed profile storage:\n before %s\n after %s", before, after)
	}
	invalid := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"Mismatched selection","modelId":"deepseek-flash","models":["second"]}`, true)
	if invalid.ok || invalid.code != apierr.CodeInvalidRequest {
		t.Fatalf("modelId outside supplied models must be rejected: %s", invalid.body)
	}
	if after := persistedProfiles(t, srv); string(after) != string(before) {
		t.Fatalf("invalid model selection changed profile storage:\n before %s\n after %s", before, after)
	}
}
