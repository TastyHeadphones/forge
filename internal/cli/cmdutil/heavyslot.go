package cmdutil

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/reliant-labs/forge/pkg/hostslot"
)

// HoldHeavySlot takes a host-wide heavy slot (pkg/hostslot) for a forge
// command that does heavy work in its own process — `forge generate`,
// `forge lint` — and returns the release. While every slot is held it waits,
// printing who holds them to stderr.
//
// Only a cancelled context fails the command. A slot directory forge cannot
// create or lock is a broken limiter, not a reason to refuse to lint or
// generate: the command warns once and runs unthrottled.
func HoldHeavySlot(ctx context.Context, stderr io.Writer) (release func(), err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := hostslot.Acquire(ctx, hostslot.Heavy, hostslot.Options{Out: stderr, Env: os.Environ()})
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "⚠️  forge: running without a heavy slot: %v\n", err)
		return func() {}, nil
	}
	return func() { _ = s.Release() }, nil
}
