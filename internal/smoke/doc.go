// Package smoke holds pinned scenarios against real public Go modules. Each
// scenario clones a repository at a fixed commit, upgrades one direct
// dependency to a fixed version through the public module proxy and
// checksum database, and checks the outcome. They need the network and a
// container engine, so they run only under the smoke build tag:
//
//	go test -tags smoke -count=1 -p 1 ./internal/smoke/
//
// The toolchain image for each target's go directive must be pulled first;
// the scenarios never pull.
package smoke
