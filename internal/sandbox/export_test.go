package sandbox

import "context"

// EmptyBuildCaches exposes the container path of RemoveBuildCaches, which a
// VM-backed engine never needs, so the integration tests exercise it on
// every engine.
func (d *Docker) EmptyBuildCaches(ctx context.Context) error { return d.emptyBuildCaches(ctx) }
