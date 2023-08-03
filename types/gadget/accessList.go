package gadget

type AccessList struct{}

func (al AccessList) Len() int {
	return 0
}

func (al AccessList) StorageKeys() int {
	return 0
}
