// Package grpcconsts holds hand-written constants that both sides of the gRPC
// wire need to agree on but that cannot live in the generated code (.proto
// options don't cover gRPC metadata keys).
package grpcconsts

// SessionHeader is the metadata key an agent sets on AgentService.Connect to
// authenticate its bidirectional stream. Value is the session id returned by
// AgentService.Register.
const SessionHeader = "x-gocdnext-session"
