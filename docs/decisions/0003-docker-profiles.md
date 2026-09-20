# 3. Container sandbox with three fixed profiles

Decision: run the Go toolchain in containers created through the engine
API under acquire, mutate, and execute profiles that tools cannot alter.

Why: the profile that runs repository code has no network, a read-only
snapshot and cache, an empty environment, and limits; the profiles that
need the network run no repository code. Separating them is what makes
the security argument simple and testable. A minimal engine client keeps
the dependency surface small and works over any Docker-compatible socket.

Alternatives: shelling out to the CLI; gVisor or Firecracker; a host
subprocess with restrictions.

Tradeoffs: engine socket access is root-equivalent on most hosts and is an
accepted risk. Egress during acquisition is restricted by configuration,
not by the operating system.

Revisit when: hostile repositories become a target, which would call for
an egress proxy on an internal network and a stronger isolation layer.
