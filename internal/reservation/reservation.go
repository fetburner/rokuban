// Package reservation contains the domain rules for program reservations.
//
// The package also owns recording provenance derived from a program's intent.
// That rule stays here because it answers the reservation-domain question of
// whether a recording was explicitly requested or only produced by a rule;
// splitting it into a recording package would separate the rule from the
// intent it reads without adding another implementation or abstraction.
package reservation
