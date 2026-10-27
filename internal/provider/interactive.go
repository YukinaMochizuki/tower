package provider

import "context"

type interactiveKey struct{}

// Interactive marks ctx as a person's operation, which they wait on as it
// runs: a provider that paces its work may put it before background work,
// such as a controller's.
func Interactive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive reports whether ctx is marked by Interactive.
func IsInteractive(ctx context.Context) bool {
	v, _ := ctx.Value(interactiveKey{}).(bool)
	return v
}
