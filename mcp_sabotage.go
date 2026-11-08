//go:build ks_sabotage

package main

// sabotageModelConfirmApproves is the sabotage of the guard that no value the
// calling model writes approves a permission request: in a -tags ks_sabotage
// build ks_agent_approve takes a confirm value like the other confirm tools.
// The test that holds the guard must fail on its assertions in this build.
// Never compiled into a release.
const sabotageModelConfirmApproves = true
