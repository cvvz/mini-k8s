package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"time"
)

// runLookup resolves a name with Go's resolver, which follows the Pod's
// /etc/resolv.conf (search domains, ndots) and /etc/hosts like libc does:
//
//	mini lookup web                  addresses, one per line
//	mini lookup -t SRV _http._tcp.web priority weight port target
//	mini lookup -t CNAME ext         the canonical name
func runLookup(args []string) int {
	fs := flag.NewFlagSet("lookup", flag.ContinueOnError)
	typ := fs.String("t", "A", "record type: A, SRV or CNAME")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini lookup [-t A|SRV|CNAME] NAME")
		return 2
	}
	name := fs.Arg(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &net.Resolver{PreferGo: true}
	var lines []string
	var err error
	switch *typ {
	case "A":
		lines, err = r.LookupHost(ctx, name)
		sort.Strings(lines)
	case "SRV":
		var srvs []*net.SRV
		if _, srvs, err = r.LookupSRV(ctx, "", "", name); err == nil {
			for _, s := range srvs {
				lines = append(lines, fmt.Sprintf("%d %d %d %s", s.Priority, s.Weight, s.Port, s.Target))
			}
			sort.Strings(lines)
		}
	case "CNAME":
		var cname string
		if cname, err = r.LookupCNAME(ctx, name); err == nil {
			lines = []string{cname}
		}
	default:
		fmt.Fprintf(os.Stderr, "unsupported record type %q\n", *typ)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}
