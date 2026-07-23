package portal

import (
	"context"
	"errors"
	"testing"
)

type fakeTokenSource struct {
	token string
	err   error
	gotID int64
}

func (f *fakeTokenSource) Token(_ context.Context, installationID int64) (string, error) {
	f.gotID = installationID
	return f.token, f.err
}

type fakeOrgAPI struct {
	member   bool
	err      error
	gotOrg   string
	gotLogin string

	// UserLoginByID script: the resolved login (idFound true), a deleted
	// account (idFound false), or a lookup failure (idErr).
	loginByID string
	idFound   bool
	idErr     error
	gotID     int64
}

func (f *fakeOrgAPI) OrgMember(_ context.Context, org, login string) (bool, error) {
	f.gotOrg, f.gotLogin = org, login
	return f.member, f.err
}

func (f *fakeOrgAPI) UserLoginByID(_ context.Context, id int64) (login string, found bool, err error) {
	f.gotID = id
	if f.idErr != nil {
		return "", false, f.idErr
	}
	return f.loginByID, f.idFound, nil
}

// orgValidatorFor wires an OrgValidator around one scripted fakeOrgAPI.
func orgValidatorFor(api *fakeOrgAPI) *OrgValidator {
	return &OrgValidator{
		Org:    "acme",
		Tokens: &fakeTokenSource{token: "t"},
		NewClient: func(string) (OrgAPI, error) {
			return api, nil
		},
	}
}

func TestOrgValidator(t *testing.T) {
	source := &fakeTokenSource{token: "inst-token"}
	api := &fakeOrgAPI{member: true, loginByID: "octocat", idFound: true}
	var gotToken string
	v := &OrgValidator{
		Org:            "acme",
		InstallationID: 99,
		Tokens:         source,
		NewClient: func(token string) (OrgAPI, error) {
			gotToken = token
			return api, nil
		},
	}
	member, err := v.Member(context.Background(), 42, "octocat")
	if err != nil || !member {
		t.Fatalf("Member = %v, %v", member, err)
	}
	if source.gotID != 99 || gotToken != "inst-token" {
		t.Errorf("installation token plumbing: id=%d token=%q", source.gotID, gotToken)
	}
	if api.gotID != 42 {
		t.Errorf("login resolved for user %d, want 42", api.gotID)
	}
	if api.gotOrg != "acme" || api.gotLogin != "octocat" {
		t.Errorf("probe args: org=%q login=%q", api.gotOrg, api.gotLogin)
	}
}

func TestOrgValidatorNonMember(t *testing.T) {
	v := orgValidatorFor(&fakeOrgAPI{member: false, loginByID: "outsider", idFound: true})
	member, err := v.Member(context.Background(), 42, "outsider")
	if err != nil || member {
		t.Fatalf("Member = %v, %v, want definitive false", member, err)
	}
}

// TestOrgValidatorLoginResolution pins the rename-safety contract: the
// membership probe runs against the login the immutable user ID currently
// resolves to, a deleted account is definitively out, and a lookup failure
// is "unknown" — never a refusal.
func TestOrgValidatorLoginResolution(t *testing.T) {
	tests := []struct {
		name       string
		api        *fakeOrgAPI
		wantMember bool
		wantErr    bool
		wantProbe  string // login OrgMember must have been probed with ("" = no probe)
	}{
		{
			name:       "renamed login probes the fresh one",
			api:        &fakeOrgAPI{member: true, loginByID: "octocat-new", idFound: true},
			wantMember: true,
			wantProbe:  "octocat-new",
		},
		{
			name: "deleted account is a definitive non-member",
			api:  &fakeOrgAPI{member: true, idFound: false},
		},
		{
			name:    "lookup failure is unknown, never a refusal",
			api:     &fakeOrgAPI{member: true, idErr: errors.New("500 from github")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			member, err := orgValidatorFor(tt.api).Member(context.Background(), 42, "octocat-stale")
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Member: %v", err)
			}
			if member != tt.wantMember {
				t.Errorf("member = %v, want %v", member, tt.wantMember)
			}
			if tt.api.gotLogin != tt.wantProbe {
				t.Errorf("probed login %q, want %q", tt.api.gotLogin, tt.wantProbe)
			}
		})
	}
}

func TestOrgValidatorErrors(t *testing.T) {
	// Token mint failure is an error, never a "false".
	v := &OrgValidator{Org: "acme", Tokens: &fakeTokenSource{err: errors.New("mint down")}}
	if _, err := v.Member(context.Background(), 42, "octocat"); err == nil {
		t.Fatal("token failure not surfaced")
	}
	// API failure likewise.
	v = orgValidatorFor(&fakeOrgAPI{err: errors.New("throttled"), loginByID: "octocat", idFound: true})
	if _, err := v.Member(context.Background(), 42, "octocat"); err == nil {
		t.Fatal("API failure not surfaced")
	}
	// Nothing to validate: no user ID and no login.
	if _, err := v.Member(context.Background(), 0, ""); err == nil {
		t.Fatal("empty identity accepted")
	}
}

func TestAccountValidator(t *testing.T) {
	v := &AccountValidator{AccountID: 42}
	if ok, err := v.Member(context.Background(), 42, "owner"); err != nil || !ok {
		t.Fatalf("owner refused: %v %v", ok, err)
	}
	if ok, err := v.Member(context.Background(), 7, "other"); err != nil || ok {
		t.Fatalf("non-owner accepted: %v %v", ok, err)
	}
}
