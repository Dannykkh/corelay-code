// Package capabilityprofile measures a provider/model target with a fixed,
// versioned probe plan and persists immutable empirical recommendations.
//
// The package deliberately does not integrate with the agent loop or harness
// resolver. AutoSelect is the only automatic-selection policy here, and it
// admits only an exact-target, naturally verified, unexpired, nonquarantined
// profile. Manual overrides do not patch recommendations; they authorize an
// existing quarantined profile only through explicit profile and override IDs.
//
// IsolationProof is an attestation boundary, not necessarily an
// operating-system sandbox for the whole host process. The production
// DisposableWorkspaceFactory accepts only an IsolationProvisioner whose
// versioned proof says every model-controlled execution path, filesystem
// access, and provider outbound request is constrained for the exact target.
// A kernel composition may satisfy that contract by disabling ambient
// executable surfaces, enforcing canonical workspace paths, denying
// model-issued processes, scoping HOME/config, and injecting a target-bound
// provider transport. The profiler validates proof completeness and target
// binding before Executor is called, but cannot independently inspect the
// provisioner's implementation.
package capabilityprofile
