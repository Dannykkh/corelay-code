// Package sandbox defines the fail-closed process isolation contract used by
// the tool safety pipeline.
//
// AutoRunner selects a probed platform adapter without degrading isolation
// requirements. UnavailableRunner never starts a process. UnconfinedRunner is
// named to make its lack of isolation explicit and runs only under
// EnforcementDisabled. Platform adapters advertise only controls they can
// actually enforce. Every executable Runner also applies the same bounded,
// combined stdout+stderr capture contract before returning process output.
package sandbox
