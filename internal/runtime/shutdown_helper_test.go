package runtime

import "testing"

// stopRuntime performs one shutdown attempt without a root-context cancel,
// with each bounded wait capped at defaultDrainTimeout. It is the test
// replacement for the removed Runtime.Stop compatibility wrapper: production
// shutdown goes through Shutdown with a root cancel, while tests drive the
// same lifecycle without one.
func stopRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	rt.Shutdown(nil, defaultDrainTimeout)
}
