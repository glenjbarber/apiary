package hoststats

import "testing"

func TestParseLoadAvg(t *testing.T) {
	l1, l5, l15, err := parseLoadAvg("{ 2.35 2.24 2.29 }\n")
	if err != nil {
		t.Fatalf("parseLoadAvg() error: %v", err)
	}
	if l1 != 2.35 || l5 != 2.24 || l15 != 2.29 {
		t.Errorf("parseLoadAvg() = (%v, %v, %v), want (2.35, 2.24, 2.29)", l1, l5, l15)
	}
}

func TestParseLoadAvg_MalformedIsError(t *testing.T) {
	if _, _, _, err := parseLoadAvg("garbage"); err == nil {
		t.Fatalf("parseLoadAvg(garbage) = nil error, want a parse failure")
	}
}

func TestParseMemInfo(t *testing.T) {
	info, err := parseMemInfo("33977335808\n", "325746\n", "4096\n")
	if err != nil {
		t.Fatalf("parseMemInfo() error: %v", err)
	}
	if info.TotalBytes != 33977335808 {
		t.Errorf("TotalBytes = %d, want 33977335808", info.TotalBytes)
	}
	if want := uint64(325746 * 4096); info.FreeBytes != want {
		t.Errorf("FreeBytes = %d, want %d", info.FreeBytes, want)
	}
}

func TestParseZpoolList(t *testing.T) {
	// Real captured output from `zpool list -Hp -o name,size,alloc,free,capacity,health`.
	out := "tank\t2989297238016\t118563520512\t2870733717504\t3\tONLINE\n" +
		"zroot\t996432412672\t6710104064\t989722308608\t0\tONLINE\n"

	pools := parseZpoolList(out)
	if len(pools) != 2 {
		t.Fatalf("parseZpoolList() = %d pools, want 2", len(pools))
	}
	if pools[0].Name != "tank" || pools[0].SizeBytes != 2989297238016 || pools[0].CapacityPct != 3 || pools[0].Health != "ONLINE" {
		t.Errorf("pools[0] = %+v, want tank/2989297238016/3%%/ONLINE", pools[0])
	}
	if pools[1].Name != "zroot" || pools[1].AllocBytes != 6710104064 {
		t.Errorf("pools[1] = %+v, want zroot with AllocBytes=6710104064", pools[1])
	}
}

func TestParseZpoolList_EmptyInput(t *testing.T) {
	if pools := parseZpoolList(""); pools != nil {
		t.Errorf("parseZpoolList(\"\") = %+v, want nil", pools)
	}
}

func TestParseSmartInfo(t *testing.T) {
	// Real captured output from `smart -i /dev/ada0`.
	out := "Device\tSamsung SSD 860 EVO 1TB\n" +
		"Revision\tSamsung SSD 860 EVO 1TB\n" +
		"Serial\tS5B3NMFN802667L\n"

	model, serial := parseSmartInfo(out)
	if model != "Samsung SSD 860 EVO 1TB" {
		t.Errorf("model = %q, want %q", model, "Samsung SSD 860 EVO 1TB")
	}
	if serial != "S5B3NMFN802667L" {
		t.Errorf("serial = %q, want %q", serial, "S5B3NMFN802667L")
	}
}

func TestParseSmartStatus_Healthy(t *testing.T) {
	out := "Reallocated Sectors Count\t0\t51\t100\t100\n" +
		"SMART Status\t0\n"

	healthy, found := parseSmartStatus(out)
	if !found {
		t.Fatalf("parseSmartStatus() found = false, want true")
	}
	if !healthy {
		t.Errorf("parseSmartStatus() healthy = false, want true for status 0")
	}
}

func TestParseSmartStatus_Unhealthy(t *testing.T) {
	out := "SMART Status\t1\n"
	healthy, found := parseSmartStatus(out)
	if !found {
		t.Fatalf("parseSmartStatus() found = false, want true")
	}
	if healthy {
		t.Errorf("parseSmartStatus() healthy = true, want false for a nonzero status")
	}
}

func TestParseSmartStatus_MissingLineNotFound(t *testing.T) {
	_, found := parseSmartStatus("Reallocated Sectors Count\t0\t51\t100\t100\n")
	if found {
		t.Errorf("parseSmartStatus() found = true, want false when no SMART Status line is present")
	}
}

func TestParseNetstat(t *testing.T) {
	// Real captured output from `netstat -ibn` (header + link/address
	// rows for two interfaces).
	out := `Name    Mtu Network              Address                                Ipkts Ierrs Idrop        Ibytes      Opkts Oerrs        Obytes  Coll
re0    1500 <Link#1>             10:bf:48:85:a3:d4                  151574243     0     0  227328955324   78992712     0    9078351364     0
re0       - fe80::%re0/64        fe80::12bf:48ff:fe85:a3d4%re0            132     -     -          8976        138     -          9444     -
lo0   16384 <Link#2>             lo0                                  2585018     0     0    7709882303    2584997     0    7709877091     0
`
	ifaces := parseNetstat(out)
	if len(ifaces) != 2 {
		t.Fatalf("parseNetstat() = %d interfaces, want 2, got %+v", len(ifaces), ifaces)
	}
	if ifaces[0].Name != "re0" || ifaces[0].RxBytes != 227328955324 || ifaces[0].TxBytes != 9078351364 {
		t.Errorf("ifaces[0] = %+v, want re0/227328955324/9078351364", ifaces[0])
	}
	if ifaces[1].Name != "lo0" || ifaces[1].RxBytes != 7709882303 {
		t.Errorf("ifaces[1] = %+v, want lo0 with RxBytes=7709882303", ifaces[1])
	}
}

func TestParseNetstat_IgnoresNonLinkRows(t *testing.T) {
	out := "10.50.0.0/24         10.50.0.14                         151561265     -     -  225200750479   78989442     -    7972176217     -\n"
	if ifaces := parseNetstat(out); len(ifaces) != 0 {
		t.Errorf("parseNetstat() = %+v, want no interfaces from a non-link row", ifaces)
	}
}
