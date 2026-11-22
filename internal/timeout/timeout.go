// Package timeout is the single source of the daemon/CLI operation budget.
package timeout

import "time"

// OperationSeconds is the budget in whole seconds, as quoted in error hints
// and documentation.
const OperationSeconds = 60

// Operation bounds one daemon RPC's CDP work. The CLI gives a whole command
// the same budget, so it never promises more than the daemon will do.
const Operation = OperationSeconds * time.Second
