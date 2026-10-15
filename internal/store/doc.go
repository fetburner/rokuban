// Package store defines domain-specific persistence contracts.
//
// Each interface models one domain, and each method owns its database transaction.
// The contracts share this package because their errors and domain types form one
// application boundary; separate packages can be added when a domain needs an
// independent import boundary.
package store
