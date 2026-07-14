package classification

// Classification represents a data classification tier per docs/architecture/04-data-model.md §20.
type Classification string

const (
	Public       Classification = "PUBLIC"
	Internal     Classification = "INTERNAL"
	Confidential Classification = "CONFIDENTIAL"
	Restricted   Classification = "RESTRICTED"
)

func (c Classification) String() string {
	return string(c)
}

func (c Classification) Valid() bool {
	switch c {
	case Public, Internal, Confidential, Restricted:
		return true
	default:
		return false
	}
}
