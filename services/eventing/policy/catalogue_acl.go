package policy

import "zoiko.io/eventing/catalogue"

// CatalogueACL is the ProducerACL the event catalogue implies: each type the
// catalogue assigns to a deployed service may be published only by that
// service (E-02). It is lenient on purpose, so a type whose catalogue producer
// is a role or a domain nobody owns yet is not enforced, instead of being
// refused until the catalogue names a service.
func CatalogueACL() (*ProducerACL, error) {
	grants, err := catalogue.ServiceGrants()
	if err != nil {
		return nil, err
	}
	return NewProducerACL(grants, false)
}
