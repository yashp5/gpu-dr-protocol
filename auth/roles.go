package auth

import (
	"encoding"
	"fmt"
	"math/bits"
	"strings"
	"sync"

	"github.com/grene-labs/grene/generated/sqlc"
)

// Role (or more accurately, permissions) within the Grene system.
// A user may have more than one role. You may combine roles using bitwise OR.
type Role uint64

// Role definitions. You may have more than one role by combining them with bitwise OR.
const (
	RoleAdmin           Role = 1 << iota                   // Admin is special role which overrides all other roles.
	RoleAll, Role_COUNT      = Role((1 << iota) - 1), iota // MUST BE LAST
	RolePublic          Role = 0
)

var (
	tableOnce sync.Once                          // guards below
	role2Name = make(map[Role]string, RoleAll+1) // initialized in initRoleTables
	name2Role = make(map[string]Role, RoleAll+1) // initialized in initRoleTables
)

func init() { go initRoleTables() } // nonblocking, eager initialization of role2Name and name2Role

// initialize the role tables, exactly once.
// This is called in a goroutine to avoid blocking the main thread and "slow" program startup.
func initRoleTables() {
	tableOnce.Do(
		func() {

			role2Name = make(map[Role]string, 1<<Role_COUNT)
			role2Name[RolePublic] = string(sqlc.UserRolePUBLIC)
			role2Name[RoleAdmin] = string(sqlc.UserRoleADMIN)
			// TODO: lender?

			name2Role = make(map[string]Role, len(role2Name))
			for r, n := range role2Name {
				name2Role[n] = r
			}
			var buf strings.Builder
			for r := Role(0); r <= RoleAll; r++ {
				if bits.OnesCount(uint(r)) <= 1 {
					continue
				}
				buf.Reset()

				for i := 0; i < Role_COUNT; i++ {
					bit := r & (1 << i)
					if bit == 0 {
						continue
					}
					if name, ok := role2Name[bit]; ok {
						if buf.Len() > 0 {
							buf.WriteByte('|')
						}
						buf.WriteString(name)
					}
				}
				s := buf.String()
				role2Name[r] = s
			}
			for r, n := range role2Name {
				name2Role[n] = r
			}
		},
	)
}

var _ encoding.TextAppender = Role(0)

// UnmarshalText parses the string representation of the role.
func (r *Role) UnmarshalText(b []byte) error {
	initRoleTables() // blocks until the role tables are initialized
	s := strings.ReplaceAll(strings.ToUpper(string(b)), " ", "")

	got, ok := name2Role[s]
	if ok {
		*r = got
		return nil
	}
	return fmt.Errorf("unknown role %q", b)
}

func RoleFromStringSlice(s []string) (r Role) {
	initRoleTables() // blocks until the role tables are initialized
	for i := range s {
		r |= name2Role[s[i]]
	}
	return r
}

// Has checks if the role has any of the specified permissions.
// This is just a wrapper around the bitwise AND operator: people aren't very familiar with bitwise ops nowadays.
func (r Role) HasAny(perms Role) bool {
	return r&perms != 0
}

// Check that the role has all of the specified permissions.
// This is just a wrapper around the bitwise AND operator: people aren't very familiar with bitwise ops nowadays.
func (r Role) HasAll(perms Role) bool {
	return r&perms == perms
}

// MarshalText returns the string representation of the role.
// This also works for JSON serialization.
func (r Role) MarshalText() ([]byte, error) {
	return r.AppendText(nil)
}

// AppendText appends the string representation of the role to b.
func (r Role) AppendText(b []byte) ([]byte, error) {
	initRoleTables() // blocks until the role tables are initialized.

	name, ok := role2Name[r]
	if !ok {
		return nil, fmt.Errorf("unknown role %d", r)
	}
	return append(b, name...), nil
}

// StringSlice returns a []string obtained as though each bit in the Role has .String() called on it.
func (r Role) StringSlice() []string {
	return strings.Split(r.String(), "|")
}

// String returns a string representation of the role.
func (r Role) String() string {
	initRoleTables() // blocks until the role tables are initialized.
	if name, ok := role2Name[r]; ok {
		return name
	}
	return fmt.Sprintf("Role(%#08b)", r)
}
