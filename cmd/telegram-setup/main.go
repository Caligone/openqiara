// Command telegram-setup finds the Telegram chat id of the owner and writes
// the configuration file read by openqiarad. Run it on a PC once, after
// sending any message to the new bot:
//
//	telegram-setup -token <TOKEN> -out openqiara_telegram.json
//
// To authorize another phone later, ask its owner to write to the bot and run:
//
//	telegram-setup -add -out openqiara_telegram.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/caligone/openqiara/internal/telegram"
)

const (
	waitMax  = 5 * time.Minute
	pollWait = 30 * time.Second
)

func main() { os.Exit(run()) }

func run() int {
	token := flag.String("token", "", "jeton du bot (obtenu auprès de @BotFather)")
	out := flag.String("out", "openqiara_telegram.json", "fichier de configuration à écrire")
	add := flag.Bool("add", false, "ajouter un compte au fichier -out existant")
	flag.Parse()

	var existing telegram.Config
	if *add {
		cfg, ok, err := telegram.LoadConfig(*out)
		if err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("%s n'existe pas", *out)
			}
			fmt.Fprintln(os.Stderr, "Erreur : impossible de lire la configuration existante :", err)
			return 1
		}
		if *token != "" && *token != cfg.BotToken {
			fmt.Fprintln(os.Stderr, "Erreur : -token diffère du jeton du fichier existant.")
			return 1
		}
		existing = cfg
		*token = cfg.BotToken
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "Erreur : l'option -token est obligatoire.")
		flag.Usage()
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitMax)
	defer cancel()
	bot := telegram.NewBot(*token, "", nil)

	fmt.Println("Envoyez un message à votre bot depuis Telegram (par exemple « bonjour »)…")
	fmt.Println("J'attends votre message (5 minutes au maximum).")

	chat, err := waitNewChat(ctx, bot, existing.ChatIDs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Erreur :", err)
		return 1
	}
	cfg := telegram.Config{BotToken: *token, ChatIDs: append(append([]int64(nil), existing.ChatIDs...), chat)}
	if err := writeConfig(*out, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Erreur : impossible d'écrire le fichier :", err)
		return 1
	}
	fmt.Printf("Message reçu du chat privé n° %d.\n", chat)
	fmt.Printf("Configuration écrite dans %s (%d compte(s) autorisé(s)).\n", *out, len(cfg.ChatIDs))

	rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rcancel()
	if err := bot.SendMessage(rctx, chat, "✅ Ce téléphone recevra les alertes.", nil); err != nil {
		fmt.Fprintln(os.Stderr, "Attention : la confirmation n'a pas pu être envoyée :", err)
	}
	fmt.Println("Étape suivante : CAM_HOST=<ip de la caméra> ./scripts/telegram_install.sh", *out)
	return 0
}

// waitNewChat long-polls until a private text message from an account not in
// known arrives, or ctx ends.
func waitNewChat(ctx context.Context, bot *telegram.Bot, known []int64) (int64, error) {
	// Drain updates queued before the tool started: only messages sent from
	// now on count.
	var offset int64
	if old, err := bot.GetUpdates(ctx, 0, 0); err == nil {
		for _, u := range old {
			if u.ID >= offset {
				offset = u.ID + 1
			}
		}
	}
	for {
		ups, err := bot.GetUpdates(ctx, offset, pollWait)
		if ctx.Err() != nil {
			return 0, fmt.Errorf("aucun message reçu en 5 minutes. Relancez la commande, puis écrivez à votre bot")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Problème de connexion à Telegram, nouvelle tentative…", err)
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
		}
		if id, ok := pickNewChat(known, ups); ok {
			return id, nil
		}
	}
}

// pickNewChat returns the chat of the first text message sent in a private
// chat (chat id equals sender id, positive) by an account not in known;
// groups, callbacks, other update kinds and known accounts are skipped.
func pickNewChat(known []int64, ups []telegram.Update) (int64, bool) {
	for _, u := range ups {
		if u.Callback == "" && u.Text != "" && u.ChatID > 0 && u.ChatID == u.FromID && !slices.Contains(known, u.ChatID) {
			return u.ChatID, true
		}
	}
	return 0, false
}

func writeConfig(path string, cfg telegram.Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps the mode of an existing file
}
