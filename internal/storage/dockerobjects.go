package storage

import (
	"context"
	"strings"
)

// Removing Docker objects — dangling images (danglingimages.go) and anonymous
// volumes (anonvolumes.go) — goes through the pass's deletion governor like
// every unlink forge makes itself (deletes.go). Each object is one grant: the
// pass's back-off, delete cap and deadline stop the layer, and
// max_docker_removals_per_pass bounds how many objects one pass hands the
// daemon. Removal is never forced, so the daemon's own in-use check is the
// last guard after forge's.

// dockerRemoval is what became of one object a layer asked Docker to remove.
type dockerRemoval int

const (
	dockerRemoved dockerRemoval = iota
	// dockerGone: the object no longer exists; something else removed it.
	dockerGone
	// dockerRefused: the daemon refused because the object is in use again,
	// by a container created after the plan was made. That is the last guard
	// working, so the object is retained and the layer does not fail.
	dockerRefused
	dockerFailed
)

// removeDockerObject asks the daemon to remove one object through the pass
// governor d. A dockerFailed error ends the layer when stopsPass(err) — a
// back-off, the pass's caps or its deadline — and is otherwise this object's
// failure alone.
func (r Runner) removeDockerObject(ctx context.Context, d deleter, what string, args ...string) (dockerRemoval, error) {
	if err := d.grantObject(ctx); err != nil {
		return dockerFailed, err
	}
	_, err := r.docker(ctx, args...)
	return classifyDockerRemoval(d, what, err)
}

// classifyDockerRemoval sorts a docker error into the outcomes above. A
// distress error backs the whole pass off (dockerDistress, observe). The rest
// can only be told apart by the daemon's message text.
func classifyDockerRemoval(d deleter, what string, err error) (dockerRemoval, error) {
	if err == nil {
		return dockerRemoved, nil
	}
	if err := d.observe(what, dockerDistress(err)); stopsPass(err) {
		return dockerFailed, err
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such image") || strings.Contains(msg, "no such volume"):
		return dockerGone, nil
	case strings.Contains(msg, "conflict") || strings.Contains(msg, "in use") || strings.Contains(msg, "being used"):
		return dockerRefused, err
	}
	return dockerFailed, err
}

// dockerLayerErr is a docker read error as a layer's result: a distress error
// backs the pass off, anything else fails the layer, and either way nothing
// was removed.
func dockerLayerErr(d deleter, err error) error {
	return d.observe("docker", dockerDistress(err))
}
