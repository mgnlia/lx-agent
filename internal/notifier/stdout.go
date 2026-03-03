package notifier

import (
	"context"
	"fmt"
)

type StdoutNotifier struct{}

func NewStdout() *StdoutNotifier {
	return &StdoutNotifier{}
}

func (s *StdoutNotifier) Send(_ context.Context, message string) error {
	fmt.Println(message)
	return nil
}
