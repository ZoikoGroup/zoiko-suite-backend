package domain

// machine is the lifecycle table, keyed by command then current state. It is
// exactly:
//
//	OPEN              --SOFT_CLOSE-->        SOFT_CLOSED
//	SOFT_CLOSED       --HARD_CLOSE-->        HARD_CLOSED
//	HARD_CLOSED       --AUTHORIZE_REOPEN-->  REOPEN_AUTHORIZED
//	REOPEN_AUTHORIZED --RECLOSE-->           RECLOSED
//	RECLOSED          --AUTHORIZE_REOPEN-->  REOPEN_AUTHORIZED  (a later authorised reopen)
//
// There is deliberately no SOFT_CLOSED -> OPEN: an un-soft-close is not a spec
// transition. Anything not in the table is INVALID_TRANSITION.
var machine = map[Command]map[State]State{
	CmdSoftClose:       {StateOpen: StateSoftClosed},
	CmdHardClose:       {StateSoftClosed: StateHardClosed},
	CmdAuthorizeReopen: {StateHardClosed: StateReopenAuthorized, StateReclosed: StateReopenAuthorized},
	CmdReclose:         {StateReopenAuthorized: StateReclosed},
}

// StateCommands are the commands that move an existing period.
var StateCommands = []Command{CmdSoftClose, CmdHardClose, CmdAuthorizeReopen, CmdReclose}

// Next returns the state a command leads to from `from`, or INVALID_TRANSITION.
func Next(cmd Command, from State) (State, error) {
	t, ok := machine[cmd]
	if !ok {
		return "", Errf(CodeInvalidTransition, "unknown period command %q", cmd)
	}
	if !from.Valid() {
		return "", Errf(CodeInvalidTransition, "unknown period state %q", from)
	}
	to, ok := t[from]
	if !ok {
		return "", Errf(CodeInvalidTransition, "command %s is not allowed from state %s", cmd, from)
	}
	return to, nil
}

// NeedsSoD reports whether the command needs an actor other than the one who
// requested the soft close.
func NeedsSoD(cmd Command) bool { return cmd == CmdHardClose || cmd == CmdAuthorizeReopen }
