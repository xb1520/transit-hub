package admin_accounts

import (
	"context"
	"testing"

	"transithub/backend/internal/shared/authctx"
)

type selectionFakeRepository struct {
	accounts      []Account
	defaultID     string
	getForUserID  string
	getForUserAcc string
}

func (f *selectionFakeRepository) EnsureSchema(ctx context.Context) error     { return nil }
func (f *selectionFakeRepository) AssignLegacyRows(ctx context.Context) error { return nil }
func (f *selectionFakeRepository) List(ctx context.Context, userID string) ([]Account, error) {
	out := make([]Account, len(f.accounts))
	copy(out, f.accounts)
	return out, nil
}
func (f *selectionFakeRepository) Current(ctx context.Context, userID string) (*Account, error) {
	for i := range f.accounts {
		if f.accounts[i].ID == f.defaultID {
			account := f.accounts[i]
			account.Current = true
			return &account, nil
		}
	}
	return nil, nil
}
func (f *selectionFakeRepository) CurrentID(ctx context.Context, userID string) (string, error) {
	return f.defaultID, nil
}
func (f *selectionFakeRepository) GetForUser(ctx context.Context, userID string, accountID string) (*Account, error) {
	f.getForUserID = userID
	f.getForUserAcc = accountID
	for i := range f.accounts {
		if f.accounts[i].ID == accountID {
			account := f.accounts[i]
			return &account, nil
		}
	}
	return nil, nil
}
func (f *selectionFakeRepository) UpsertAndSwitch(ctx context.Context, userID string, input UpsertInput) (Account, error) {
	return Account{}, nil
}
func (f *selectionFakeRepository) Switch(ctx context.Context, userID string, accountID string) (*Account, error) {
	return nil, nil
}
func (f *selectionFakeRepository) Update(ctx context.Context, userID string, accountID string, displayName string) (*Account, error) {
	return nil, nil
}
func (f *selectionFakeRepository) DeleteWorkspace(ctx context.Context, userID string, accountID string) (*DeleteResult, error) {
	return nil, nil
}
func (f *selectionFakeRepository) ClaimDueCleanupJobs(ctx context.Context, limit int) ([]CleanupJob, error) {
	return nil, nil
}
func (f *selectionFakeRepository) CompleteCleanupJob(ctx context.Context, id string) error {
	return nil
}
func (f *selectionFakeRepository) MarkCleanupJobRetry(ctx context.Context, id string, attempt int, err error) error {
	return nil
}

func TestCurrentIDPrefersBrowserLocalHeader(t *testing.T) {
	repo := &selectionFakeRepository{
		defaultID: "account-db",
		accounts: []Account{
			{ID: "account-db", DisplayName: "DB Default"},
			{ID: "account-browser", DisplayName: "Browser Local"},
		},
	}
	service := NewService(repo)
	ctx := authctx.WithAdminAccountID(context.Background(), "account-browser")

	id, err := service.CurrentID(ctx, "user-1")
	if err != nil {
		t.Fatalf("CurrentID: %v", err)
	}
	if id != "account-browser" {
		t.Fatalf("expected browser-local workspace, got %q", id)
	}
	if repo.getForUserID != "user-1" || repo.getForUserAcc != "account-browser" {
		t.Fatalf("expected ownership check, got user=%q account=%q", repo.getForUserID, repo.getForUserAcc)
	}
}

func TestCurrentIDFallsBackWhenHeaderWorkspaceMissing(t *testing.T) {
	repo := &selectionFakeRepository{
		defaultID: "account-db",
		accounts: []Account{
			{ID: "account-db", DisplayName: "DB Default"},
		},
	}
	service := NewService(repo)
	ctx := authctx.WithAdminAccountID(context.Background(), "deleted-workspace")

	id, err := service.CurrentID(ctx, "user-1")
	if err != nil {
		t.Fatalf("CurrentID: %v", err)
	}
	if id != "account-db" {
		t.Fatalf("expected DB fallback, got %q", id)
	}
}

func TestListMarksBrowserLocalWorkspaceCurrent(t *testing.T) {
	repo := &selectionFakeRepository{
		defaultID: "account-db",
		accounts: []Account{
			{ID: "account-db", DisplayName: "DB Default", Current: true},
			{ID: "account-browser", DisplayName: "Browser Local"},
			{ID: "account-other", DisplayName: "Other"},
		},
	}
	service := NewService(repo)
	ctx := authctx.WithAdminAccountID(context.Background(), "account-browser")

	accounts, err := service.List(ctx, "user-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(accounts))
	}
	if accounts[0].ID != "account-browser" || !accounts[0].Current {
		t.Fatalf("expected browser-local workspace first and current, got %+v", accounts[0])
	}
	for i := 1; i < len(accounts); i++ {
		if accounts[i].Current {
			t.Fatalf("only one workspace should be current, got current on %s", accounts[i].ID)
		}
	}
}

func TestCurrentUsesBrowserLocalHeader(t *testing.T) {
	repo := &selectionFakeRepository{
		defaultID: "account-db",
		accounts: []Account{
			{ID: "account-db", DisplayName: "DB Default"},
			{ID: "account-browser", DisplayName: "Browser Local"},
		},
	}
	service := NewService(repo)
	ctx := authctx.WithAdminAccountID(context.Background(), "account-browser")

	account, err := service.Current(ctx, "user-1")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if account == nil || account.ID != "account-browser" || !account.Current {
		t.Fatalf("expected browser-local current account, got %+v", account)
	}
}
