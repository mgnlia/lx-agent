package notifier

import "context"

// Notifier delivers messages to the user.
type Notifier interface {
	Send(ctx context.Context, message string) error
}
