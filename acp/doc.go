// Package acp runs coding sessions for an editor over the Agent Client
// Protocol (https://agentclientprotocol.com), version 1: JSON-RPC 2.0 on
// the agent's stdio, one message per line. Zed speaks it natively; VS Code
// through an ACP client extension.
//
// It is a frontend like tui and rpc: the session's rules apply, and nothing
// here decides what may run. The sandbox is what confines the agent, so
// malachi asks the editor no per-tool permission and uses neither its file
// system nor its terminal. It imports coding and agent only.
package acp
