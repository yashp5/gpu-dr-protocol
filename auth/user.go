// Package auth contains the user authentication and authorization logic for the Dora Network.
// Users have a unique ID and a role that determines their permissions.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/grene-labs/grene/dbutil"

	"github.com/google/uuid"
	"github.com/grene-labs/grene/generated/sqlc"
	"github.com/grene-labs/grene/http/reqparam"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// User for authentication purposes. The "public" user has no ID and has only the "public" role.
type User struct {
	ID       uuid.UUID `json:"id"`        // Unique identifier for the user, can be nil for the public user.
	Role     Role      `json:"role"`      // Role of the user, which determines their permissions. The public user has only the public role.
	TenantID uuid.UUID `json:"tenant_id"` // The tenant user belongs to
}

// PublicUser is a special user that represents the public user with no ID and only the public role.
var PublicUser = &User{
	ID:   uuid.Nil,
	Role: RolePublic,
}

type userkey struct{}
type principalkey struct{}

// SaveUserToCtx saves the user to the context for later retrieval via UserFromCtx or UserFromCtxOrPublic.
// It panics on a nil user.
func SaveUserToCtx(ctx context.Context, u *User) context.Context {
	if u == nil {
		panic("cannot save nil user to context")
	}
	return context.WithValue(ctx, userkey{}, u)
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

// UserFromCtxOrPublic retrieves a previously saved user from the context, or the public user if not found.
// See SaveUserToCtx for how to save a user to the context.
func UserFromCtxOrPublic(ctx context.Context) *User {
	u, ok := ctx.Value(userkey{}).(*User)
	if !ok {
		return PublicUser
	}
	return u
}

// getPrincipalFromToken verifies ID token and saves another relevant values to the Principal structure.
func getPrincipalFromToken(auth JWTAuth, projectID string, r *http.Request) (*Principal, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, errors.New("the Authorization header should look like `Bearer <token>`")
	}
	raw := strings.TrimPrefix(h, "Bearer ")
	principal, err := auth.VerifyIDToken(r.Context(), projectID, raw)
	if err != nil {
		return nil, err
	}

	return &principal, nil
}

// SavePrincipalToCtx saves the principal to the context for later retrieval via UserFromCtx or UserFromCtxOrPublic.
// It panics on a nil user.
// This is only supposed to be user for `POST /v1/users`
func SavePrincipalToCtx(ctx context.Context, u *Principal) context.Context {
	if u == nil {
		panic("cannot save nil principal to context")
	}
	return context.WithValue(ctx, principalkey{}, u)
}

// PrincipalFromCtx retrieves the principal from the context, returning a boolean indicating success.
// See SavePrincipalToCtx for how to save a user to the context.
// This is only supposed to be user for `POST /v1/users`
func PrincipalFromCtx(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalkey{}).(*Principal)
	return p, ok
}

func InjectPrincipalMiddleware(
	next http.Handler,
	auth JWTAuth,
	projectID string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := getPrincipalFromToken(auth, projectID, r)
		if err != nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("unauthorized: %w", err), http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(SavePrincipalToCtx(r.Context(), principal)))
	}
}

func InjectUserAuthMiddleware(
	next http.Handler,
	getUserByIDAndTenantID func(
		ctx context.Context,
		arg sqlc.GetUserByIDAndTenantIDParams,
	) (sqlc.GetUserByIDAndTenantIDRow, error),
	auth JWTAuth,
	projectID string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := getPrincipalFromToken(auth, projectID, r)
		if err != nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("unauthorized: %w", err), http.StatusUnauthorized)
			return
		}

		user, err := getUserByIDAndTenantID(
			r.Context(),
			sqlc.GetUserByIDAndTenantIDParams{
				ID:       pgtype.UUID{Bytes: uuid.MustParse(principal.UID), Valid: true},
				TenantID: pgtype.UUID{Bytes: uuid.MustParse(principal.TenantID), Valid: true},
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			writeresp.JSONErr(r.Context(), w, errors.New("unauthorized: user not found"), http.StatusUnauthorized)
			return
		}
		if err != nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("internal error: %w", err), http.StatusInternalServerError)
			return
		}
		if !user.TenantIsActive {
			writeresp.JSONErr(
				r.Context(), w, errors.New("unauthorized: user with inactive tenant"),
				http.StatusUnauthorized,
			)
			return
		}
		if user.DisabledAt.Valid {
			writeresp.JSONErr(r.Context(), w, errors.New("unauthorized: user is disabled"), http.StatusUnauthorized)
			return
		}
		if user.ClosedAt.Valid {
			writeresp.JSONErr(r.Context(), w, errors.New("unauthorized: user is closed"), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(
			w, r.WithContext(
				SaveUserToCtx(
					r.Context(), &User{
						ID:       user.ID.Bytes,
						Role:     RoleFromStringSlice(user.Roles),
						TenantID: user.TenantID.Bytes,
					},
				),
			),
		)
	}
}

// CheckUserRoleMiddleware checks if the injected user (see InjectUserFromAuthMiddle and InjectUserTestMiddleware) has the required role to access the next handler.
// If the user does not have the required role, it returns a 403 Forbidden response.
// If the user has no credentials at all (i.e, the public user), it returns a 401 Unauthorized response unless the role is RolePublic.
func CheckUserRoleMiddleware(
	next http.Handler,
	allowed Role,
	getUserByIDAndTenantID func(
		ctx context.Context,
		arg sqlc.GetUserByIDAndTenantIDParams,
	) (sqlc.GetUserByIDAndTenantIDRow, error),
) http.HandlerFunc {
	if allowed == RolePublic { // no-op: public user can access everything
		return next.ServeHTTP
	}
	return func(w http.ResponseWriter, r *http.Request) { // there is some restriction on access.
		user, ok := UserFromCtx(r.Context())
		if !ok || user == nil {
			writeresp.JSONErr(
				r.Context(), w, errors.New("unauthorized: no user found in context"),
				http.StatusUnauthorized,
			)
			return
		}
		logger.FromCtx(r.Context()).Info("check_user_role: checking user", "user_id", user.ID, "user_role", user.Role)

		if user.Role&RoleAdminPowers != 0 { // Admins can do anything. Warn about the access and skip to the end.
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
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("forbidden: user hasn't required role"), http.StatusForbidden)
			return
		}
		// if this is an integrator access, we ensure integrator's and user's tenant_id match
		if allowed&RoleIntegrator != 0 {
			userID, err := reqparam.Path(r, "user_id", uuid.Parse)
			if err != nil {
				var pe *reqparam.ParamError
				if errors.As(err, &pe) && errors.Is(pe.Err, reqparam.ErrNoParam) {
					// this is a route that an Integrator can access to, and there is no user_id parameter
				} else {
					writeresp.JSONErr(r.Context(), w, err, http.StatusBadRequest)
					return
				}
			} else {
				_, err = getUserByIDAndTenantID(
					r.Context(), sqlc.GetUserByIDAndTenantIDParams{
						ID:       dbutil.UUIDToDB(userID),
						TenantID: dbutil.UUIDToDB(user.TenantID),
					},
				)
				if err != nil {
					writeresp.JSONErr(
						r.Context(),
						w,
						fmt.Errorf("user from path doesn't belong to the tenant %s", user.TenantID.String()),
						http.StatusForbidden,
					)
					return
				}
			}
		}
		// is this a "self-only" access? If so, check if the user is trying to access their own data by seeing if the {user_id} path parameter matches their user ID.
		if allowed&RoleSelf != 0 && user.ID != uuid.Nil {
			userID, err := reqparam.Path(r, "user_id", uuid.Parse)
			if err != nil {
				var pe *reqparam.ParamError
				if errors.As(err, &pe) && pe.Err == reqparam.ErrNoParam {
					// do nothing: self endpoint
				} else {
					writeresp.JSONErr(r.Context(), w, err, http.StatusBadRequest)
					return
				}
			} else if user.ID != userID {
				logger.FromCtx(r.Context()).Warn(
					fmt.Sprintf("%s %s: self access required", r.Method, r.URL),
					"user_id",
					user.ID,
					"user_role",
					user.Role,
				)
				writeresp.JSONErr(r.Context(), w, fmt.Errorf("forbidden: self access required"), http.StatusForbidden)
				return
			}
		}
	SERVE:
		next.ServeHTTP(w, r)
	}
}

// DORA-3200:
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
		logger.FromCtx(r.Context()).Info("believe_user: checking user", "user_id", userID)
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
		logger.FromCtx(r.Context()).Info("believe_user: checking user", "user_id", userID, "tenant_id", user.TenantID)
		next.ServeHTTP(
			w, r.WithContext(
				SavePrincipalToCtx(
					SaveUserToCtx(
						r.Context(), &User{
							ID:       user.ID.Bytes,
							Role:     RoleFromStringSlice(user.Roles),
							TenantID: user.TenantID.Bytes,
						},
					),
					&Principal{
						UID:      uuid.Must(uuid.NewV7()).String(),
						TenantID: user.TenantID.String(),
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

// InjectPrincipalTestMiddleware always injects the provided principal user into the request context.
// Needed for `POST /v1/user` route
func InjectPrincipalTestMiddleware(principal *Principal, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(SavePrincipalToCtx(r.Context(), principal)))
	}
}
