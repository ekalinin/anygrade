package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
	"github.com/ekalinin/anygrade/internal/webhook"
)

// newWebhookSink wires the optional completion webhook (SPEC §16), or returns
// nil - and then the binary makes no outbound HTTP request of any kind.
//
// The switch is the operator's environment, not course.yaml. The target is a
// destination and belongs to the teacher, who moves it with a push; the signing
// secret is a credential and cannot live in the repo every student clones (SPEC
// §11). Making the secret the enabling condition falls out of the same split
// and is worth stating on its own: a course.yaml the teacher controls should
// not be able to make somebody else's server start talking outward, and a
// delivery nobody can sign is one the receiver cannot tell from anyone else's.
// So a target without a secret is refused rather than sent unsigned, loudly.
func newWebhookSink(holder *intake.Holder, db store.Store, log *slog.Logger, logw io.Writer) *webhook.Sink {
	secret := os.Getenv(webhook.SecretEnv)
	if secret == "" {
		if holder.Get().Resolved.Course.Webhook.URL != "" {
			fmt.Fprintf(logw, "anygrade: warning: course.yaml sets webhook.url but %s is unset; "+
				"no events will be delivered\n", webhook.SecretEnv)
		}
		return nil
	}
	return &webhook.Sink{
		// Read through the holder rather than off the startup snapshot: a
		// teacher metadata push swaps the course, and the target follows it
		// without a restart like every other course.yaml key.
		Target: func() string { return holder.Get().Resolved.Course.Webhook.URL },
		Secret: []byte(secret),
		Login: func(ctx context.Context, userID int64) (string, error) {
			u, err := db.GetUserByID(ctx, userID)
			return u.Login, err
		},
		AllowPrivate: os.Getenv(webhook.AllowPrivateEnv) != "",
		Log:          log,
	}
}

// webhookNotifier adapts the deliverer to queue.Notifier. The translation lives
// in the composition root because it is the only place allowed to know both
// sides: queue stays free of HTTP, and webhook stays free of the queue.
type webhookNotifier struct{ sink *webhook.Sink }

func (n webhookNotifier) Completed(c queue.Completion) {
	n.sink.Send(webhook.Event{
		Kind: webhook.EventCompleted, SubID: c.SubID, UserID: c.UserID,
		TaskID: c.TaskID, Status: c.Status,
		Raw: c.Raw, Penalty: c.Penalty, Final: c.Final,
	})
}
