package judge

// TimeSynced is what the console of a Node whose spec.time names server shows of init's time sync
// (ADR 0021): the clock stepped from that server, before init wrote the authority and issued any
// certificate, and no line saying the Node's time is not synchronised.
func TimeSynced(server string) []Expect {
	return []Expect{
		{
			What:      "init stepped the clock from " + server + " before it issued certificates",
			Fragments: []string{"time synchronised", "server=" + server, "correction=step"},
			Before:    []string{"authority written"},
		},
		{What: "init did not log that time is not synchronised", Fragments: []string{"time is not synchronised"}, Absent: true},
		{What: "init did not warn that no server answered", Fragments: []string{"time not synchronised"}, Absent: true},
	}
}
