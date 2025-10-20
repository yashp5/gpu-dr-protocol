// Package auth contains the user authentication and authorization logic for the Dora Network.
// Users have a unique ID and a role that determines their permissions.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/grene-labs/grene/generated/sqlc"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// User for authentication purposes. The "public" user has no ID and has only the "public" role.
type User struct {
	ID   uuid.UUID `json:"id"`   // Unique identifier for the user, can be nil for the public user.
	Role Role      `json:"role"` // Role of the user, which determines their permissions. The public user has only the public role.
}

// PublicUser is a special user that represents the public user with no ID and only the public role.
var PublicUser = &User{
	ID:   uuid.Nil,
	Role: RolePublic,
}

type userkey struct{}

// SaveUserToCtx saves the user to the context for later retrieval via UserFromCtx or UserFromCtxOrPublic.
// It panics on a nil user.
func SaveUserToCtx(ctx context.Context, u *User) context.Context {
	if u == nil {
		panic("cannot save nil user to context")
	}
	return context.WithValue(ctx, userkey{}, u)
}

// UserFromCtxOrPublic retrieves a previously saved user from the context, or the public user if not found.
// See SaveUserToCtx for how to save a user to the context.
func UserFromCtxOrPublic(ctx context.Context) *User {
	u, ok := ctx.Value(userkey{}).(*User)
	if !ok {
		return PublicUser
	}
	return u
}

// UserFromCtx retrieves the user from the context, returning a boolean indicating success.
// See SaveUserToCtx for how to save a user to the context.
func UserFromCtx(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(userkey{}).(*User)
	if !ok {
		return PublicUser, false
	}
	return u, true
}

// CheckUserRoleMiddleware checks if the injected user (see InjectUserFromAuthMiddle and InjectUserTestMiddleware) has the required role to access the next handler.
// If the user does not have the required role, it returns a 403 Forbidden response.
// If the user has no credentials at all (i.e, the public user), it returns a 401 Unauthorized response unless the role is RolePublic.
func CheckUserRoleMiddleware(next http.Handler, allowed Role) http.HandlerFunc {
	if allowed == RolePublic { // no-op: public user can access everything
		return next.ServeHTTP
	}
	return func(w http.ResponseWriter, r *http.Request) { // there is some restriction on acces.
		user, ok := UserFromCtx(r.Context())
		if !ok || user == nil {
			writeresp.JSONErr(r.Context(), w, errors.New("unauthorized"), http.StatusUnauthorized)
			return
		}
		logger.FromCtx(r.Context()).Info("check_user_role: checking user", "user_id", user.ID, "user_role", user.Role)

		if user.Role&RoleAdmin != 0 { // Admins can do anything. Warn about the access and skip to the end.
			logger.FromCtx(r.Context()).Warn(
				fmt.Sprintf("%s %s: admin access", r.Method, r.URL),
				"user_id",
				user.ID,
				"user_role",
				user.Role,
			)
			goto SERVE
		}
		if user.Role&allowed == 0 { // Do they have any of the required roles? If this is public, we've already checked above.
			logger.FromCtx(r.Context()).Warn(
				fmt.Sprintf("%s %s: forbidden access", r.Method, r.URL),
				"user_id",
				user.ID,
				"user_role",
				user.Role,
			)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

	SERVE:
		next.ServeHTTP(w, r)
	}
}

// this middleware grabs the userID and password from the request headers,
// looks it up in the DB, and injects that into the request context.
// The password will always be "password" - if not, give a 403 forbidden.
// This should always be exclusive with our 'real' authentication & never be allowed in production or staging, but will be helpful for local & development builds.
func BelieveClientAboutUserMiddleware(
	next http.Handler,
	getUserByID func(ctx context.Context, id pgtype.UUID) (sqlc.GetUserByIDRow, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("UserID")
		/*logger.FromCtx(r.Context()).Debug(fmt.Sprintf("believe_client_auth: checking user: %s", userID))*/
		if userID == "" {
			next.ServeHTTP(w, r)
			return
		}
		id, err := uuid.Parse(userID)
		if err != nil {
			writeresp.JSONErr(
				r.Context(),
				w,
				fmt.Errorf("believe_client_auth: expected user to be a UUID, but got %s", r.URL.User.Username()),
				http.StatusForbidden,
			)
			return
		}
		pw := r.Header.Get("Password")
		if pw != "password" {
			writeresp.JSONErr(
				r.Context(),
				w,
				fmt.Errorf("believe_client_auth: expected password = 'password', got %s", pw),
				http.StatusForbidden,
			)
			return
		}
		user, err := getUserByID(r.Context(), pgtype.UUID{Bytes: id, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("unknown user %s", id), http.StatusUnauthorized)
			return
		}
		/*logger.FromCtx(r.Context()).Debug(
			fmt.Sprintf(
				"believe_client_auth: saving user: %s, role: %d",
				userID,
				RoleFromStringSlice(user.Roles),
			),
		)
		for _, role := range user.Roles {
			logger.FromCtx(r.Context()).Debug(fmt.Sprintf("believe_client_auth: user %s has role %s", userID, role))
		}*/
		// convert []sqlc.UserRole to []string so RoleFromStringSlice accepts it
		userRoles := make([]string, len(user.Roles))
		for i, r := range user.Roles {
			userRoles[i] = string(r)
		}
		next.ServeHTTP(
			w, r.WithContext(
				SaveUserToCtx(
					r.Context(), &User{
						ID:   user.ID.Bytes,
						Role: RoleFromStringSlice(userRoles),
					},
				),
			),
		)
	}
}

// InjectUserTestMiddleware always injects the provided user into the request context.
// This is useful for testing purposes where you want to simulate a specific user but don't want to spin up the whole auth system.
func InjectUserTestMiddleware(user *User, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(SaveUserToCtx(r.Context(), user)))
	}
}
