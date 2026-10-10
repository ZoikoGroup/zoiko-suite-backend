package periodgate_test

func releaseAll(c chan struct{}) { close(c) }
