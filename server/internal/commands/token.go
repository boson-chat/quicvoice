package commands

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
)

var (
	tokenUser  string
	tokenName  string
	tokenRole  string
	tokenRooms []string
	tokenTTL   time.Duration
	// Network roles / perms claims; only set when the flag is given, so
	// tokens without them still look like pre-network-roles tokens.
	tokenNetworkRoles []string
	tokenPerms        []string
)

// token mints a room token signed with JWT_SECRET, for testing against a real
// server. In production the app backend mints these.
var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Mint a test room token (HS256, signed with $JWT_SECRET)",
	RunE: func(cmd *cobra.Command, args []string) error {
		secret := os.Getenv("JWT_SECRET")
		if secret == "" {
			return errors.New("JWT_SECRET is not set")
		}
		if tokenUser == "" {
			return errors.New("--user is required")
		}
		now := time.Now()
		claims := auth.Claims{
			Name:  tokenName,
			Role:  tokenRole,
			Rooms: tokenRooms,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   tokenUser,
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
			},
		}
		if cmd.Flags().Changed("network-role") {
			claims.Roles = &tokenNetworkRoles
		}
		if cmd.Flags().Changed("perm") {
			claims.Perms = &tokenPerms
		}
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		if err != nil {
			return err
		}
		fmt.Println(s)
		return nil
	},
}

func init() {
	tokenCmd.Flags().StringVar(&tokenUser, "user", "", "user id (sub)")
	tokenCmd.Flags().StringVar(&tokenName, "name", "", "display name (defaults to user id)")
	tokenCmd.Flags().StringVar(&tokenRole, "role", "speaker", "listener | speaker | moderator | admin")
	tokenCmd.Flags().StringSliceVar(&tokenRooms, "room", nil, "allowed room (repeatable; none = any)")
	tokenCmd.Flags().StringSliceVar(&tokenNetworkRoles, "network-role", nil, "network role for the roles claim (repeatable; --network-role= for an empty claim)")
	tokenCmd.Flags().StringSliceVar(&tokenPerms, "perm", nil, "permission for the perms claim, e.g. voice.create (repeatable; --perm= for an empty claim)")
	tokenCmd.Flags().DurationVar(&tokenTTL, "ttl", time.Hour, "token lifetime")
	rootCmd.AddCommand(tokenCmd)
}
