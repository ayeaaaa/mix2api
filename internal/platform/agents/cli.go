package agents

import _ "embed"

// StandaloneConfigurator is the v1-compatible client-side configurator served
// at GET /agents/cli. It runs on the machine that holds the client configs and
// needs only the Python standard library.
//
//go:embed standalone/wb_agents.py
var StandaloneConfigurator []byte
