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
}

func (f *fakeOrgAPI) OrgMember(_ context.Context, org, login string) (bool, error) {
	f.gotOrg, f.gotLogin = org, login
	return f.member, f.err
}

func TestOrgValidator(t *testing.T) {
	source := &fakeTokenSource{token: "inst-token"}
	api := &fakeOrgAPI{member: true}
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
	if api.gotOrg != "acme" || api.gotLogin != "octocat" {
		t.Errorf("probe args: org=%q login=%q", api.gotOrg, api.gotLogin)
	}
}

func TestOrgValidatorNonMember(t *testing.T) {
	v := &OrgValidator{
		Org:    "acme",
		Tokens: &fakeTokenSource{token: "t"},
		NewClient: func(string) (OrgAPI, error) {
			return &fakeOrgAPI{member: false}, nil
		},
	}
	member, err := v.Member(context.Background(), 42, "outsider")
	if err != nil || member {
		t.Fatalf("Member = %v, %v, want definitive false", member, err)
	}
}

func TestOrgValidatorErrors(t *testing.T) {
	// Token mint failure is an error, never a "false".
	v := &OrgValidator{Org: "acme", Tokens: &fakeTokenSource{err: errors.New("mint down")}}
	if _, err := v.Member(context.Background(), 42, "octocat"); err == nil {
		t.Fatal("token failure not surfaced")
	}
	// API failure likewise.
	v = &OrgValidator{
		Org:    "acme",
		Tokens: &fakeTokenSource{token: "t"},
		NewClient: func(string) (OrgAPI, error) {
			return &fakeOrgAPI{err: errors.New("throttled")}, nil
		},
	}
	if _, err := v.Member(context.Background(), 42, "octocat"); err == nil {
		t.Fatal("API failure not surfaced")
	}
	// An empty login can't be probed.
	if _, err := v.Member(context.Background(), 42, ""); err == nil {
		t.Fatal("empty login accepted")
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
