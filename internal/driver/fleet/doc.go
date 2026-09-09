// Package fleet is the synthetic client driver: N clients with their own
// identities, session lifecycles and request patterns, driven against one
// subject for hours.
//
// Not built in v0. It is the load generator soak mode needs, and it is
// honestly more work than the proxy or hostile-server drivers: a proxy relays
// what it is given, while a fleet has to originate plausible traffic, model
// think time and reconnect backoff, and stay well behaved enough that anything
// the subject does wrong is attributable to the subject.
//
// v0 finds bugs; v1 finds leaks. See docs/design/soak.md.
package fleet
