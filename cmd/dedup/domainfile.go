package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// domainSet is a domain list requests use while a file may replace it, as
// SIGHUP does for the decoder's origins and the front's reverse origins.
// Adding an upstream therefore restarts neither end.
type domainSet struct {
	m atomic.Pointer[domainList]
}

// newDomainSet returns a set holding list; an empty list accepts nothing.
func newDomainSet(list domainList) *domainSet {
	s := &domainSet{}
	s.set(list)
	return s
}

func (s *domainSet) set(list domainList) {
	if list == nil {
		// A nil slice is the zero value of domainList; keep "empty list",
		// which accepts nothing, apart from "no list yet".
		list = domainList{}
	}
	s.m.Store(&list)
}

// load replaces the list with the one in path. A file that does not parse
// leaves the list in use as it was.
func (s *domainSet) load(path string) error {
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	list, err := parseDomainFile(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.set(list)
	return nil
}

// get returns nil while no list was ever set, which then restricts nothing.
func (s *domainSet) get() domainList {
	if p := s.m.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *domainSet) match(name string) bool {
	return s.get().match(name)
}

// has reports whether name is on the list itself, not only under a listed
// domain: the reverse entry forwards to exactly the hosts it names.
func (s *domainSet) has(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, d := range s.get() {
		if name == d {
			return true
		}
	}
	return false
}

// String is the list as /health advertises it.
func (s *domainSet) String() string {
	return strings.Join(s.get(), ",")
}

// parseDomainFile reads one domain per line, # starting a comment, the same
// format as the reverse origins file. An empty file is a list accepting
// nothing.
func parseDomainFile(text string) (domainList, error) {
	list := domainList{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		for _, field := range strings.Fields(line) {
			d, err := parseDomains(field)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			list = append(list, d...)
		}
	}
	return list, sc.Err()
}
