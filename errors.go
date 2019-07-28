package main

type UnstableTradeConditionError struct {
	msg string
}

func (err UnstableTradeConditionError) Error() string {
	return err.msg
}
