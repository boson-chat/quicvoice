package commands

import (
	"fmt"
	"os"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/services/liveconfig"
	"github.com/spf13/cobra"
)

var exportConfigCmd = &cobra.Command{
	Use:   "export-config",
	Short: "Print the stack config file's voice: section built from the current env (and BOSON_CONFIG, if set)",
	Long: `Prints a voice: section for config/boson.yaml from the current
environment (VOICE_ROOMS, VOICE_ROOMS_STRICT, VOICE_ROOMS_MAX_PER_USER, ...)
and the config file's own voice keys when BOSON_CONFIG points at one. rooms,
rooms_strict and max_rooms_per_user are always written; the other keys only
when their env var (or file key) is set, so the file doesn't pin defaults.
Merge it into config/boson.yaml on an existing install.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		base, err := config.LoadConfig()
		if err != nil {
			return err
		}
		f, err := config.ReadVoiceFile(base.AppConfig.ConfigFile)
		if err != nil {
			return err
		}
		defs, err := liveconfig.StaticDefs(base, f)
		if err != nil {
			return err
		}
		rooms := make([]config.RoomEntry, len(defs))
		for i, d := range defs {
			rooms[i] = config.RoomEntry{ID: d.ID, Name: d.Name}
		}
		out, err := config.ExportVoice(f.Effective(base), f, rooms, os.LookupEnv)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), string(out))
		return err
	},
}

func init() {
	rootCmd.AddCommand(exportConfigCmd)
}
