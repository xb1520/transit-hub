package authctx

import "context"

// AdminAccountIDHeader is the browser-local workspace selection header.
// Different browser frontends may send different values for the same user so
// concurrent sessions can operate on independent workspaces.
const AdminAccountIDHeader = "X-Admin-Account-Id"

type userIDKey struct{}
type adminAccountIDKey struct{}

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

func UserID(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDKey{}).(string)
	return userID, ok && userID != ""
}

func WithAdminAccountID(ctx context.Context, adminAccountID string) context.Context {
	return context.WithValue(ctx, adminAccountIDKey{}, adminAccountID)
}

func AdminAccountID(ctx context.Context) (string, bool) {
	adminAccountID, ok := ctx.Value(adminAccountIDKey{}).(string)
	return adminAccountID, ok && adminAccountID != ""
}
