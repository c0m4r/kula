package sysinfo

import "strconv"

// Bus identifiers are resolved from bundled tables rather than a pci.ids or
// usb.ids database. Those files live outside the paths the Landlock sandbox
// allows, and shelling out to lspci/lsusb is never an option. Unknown IDs fall
// back to the raw hexadecimal pair so nothing is silently invented.

var pciVendors = map[uint64]string{
	0x1000: "Broadcom / LSI",
	0x1002: "AMD / ATI",
	0x100b: "National Semiconductor",
	0x1013: "Cirrus Logic",
	0x1014: "IBM",
	0x1022: "AMD",
	0x1028: "Dell",
	0x102f: "Toshiba",
	0x1033: "NEC",
	0x1039: "Silicon Integrated Systems",
	0x103c: "Hewlett-Packard",
	0x1043: "ASUSTeK Computer",
	0x104c: "Texas Instruments",
	0x1050: "Winbond Electronics",
	0x1057: "Motorola",
	0x106b: "Apple",
	0x1077: "QLogic",
	0x1095: "Silicon Image",
	0x10b7: "3Com",
	0x10b9: "ULi Electronics",
	0x10de: "NVIDIA",
	0x10ec: "Realtek Semiconductor",
	0x1106: "VIA Technologies",
	0x111d: "Microchip / IDT",
	0x1137: "Cisco Systems",
	0x1166: "Broadcom",
	0x1180: "Ricoh",
	0x11ab: "Marvell Technology",
	0x11ad: "Lite-On Communications",
	0x11c1: "LSI",
	0x11f8: "Microsemi / PMC-Sierra",
	0x1200: "Trident Microsystems",
	0x1217: "O2 Micro",
	0x1234: "QEMU",
	0x1274: "Ensoniq",
	0x12d8: "Pericom Semiconductor",
	0x1317: "ADMtek",
	0x13b5: "ARM",
	0x13f6: "C-Media Electronics",
	0x1414: "Microsoft",
	0x14c3: "MediaTek",
	0x14e4: "Broadcom",
	0x1524: "ENE Technology",
	0x15ad: "VMware",
	0x15b3: "Mellanox Technologies",
	0x15bc: "Agilent Technologies",
	0x168c: "Qualcomm Atheros",
	0x16c3: "Synopsys",
	0x177d: "Marvell / Cavium",
	0x17cb: "Qualcomm",
	0x17d3: "Areca Technology",
	0x1814: "Ralink Technology",
	0x1912: "Renesas Technology",
	0x1924: "Solarflare Communications",
	0x1957: "Freescale Semiconductor",
	0x1969: "Qualcomm Atheros",
	0x197b: "JMicron Technology",
	0x19a2: "Emulex",
	0x19e5: "Huawei Technologies",
	0x1a03: "ASPEED Technology",
	0x1ab8: "Parallels",
	0x1ae0: "Google",
	0x1af4: "Red Hat",
	0x1b21: "ASMedia Technology",
	0x1b36: "Red Hat",
	0x1b4b: "Marvell Technology",
	0x1bb1: "Seagate Technology",
	0x1c5c: "SK hynix",
	0x1cc1: "ADATA Technology",
	0x1d0f: "Amazon",
	0x1d87: "Rockchip",
	0x1def: "Ampere Computing",
	0x1e0f: "KIOXIA",
	0x1e49: "Yangtze Memory Technologies",
	0x1e4b: "MAXIO Technology",
	0x5333: "S3 Graphics",
	0x8086: "Intel",
	0x8087: "Intel",
	0x80ee: "Oracle VirtualBox",
}

var usbVendors = map[uint64]string{
	0x03f0: "Hewlett-Packard",
	0x0403: "Future Technology Devices",
	0x0424: "Microchip Technology",
	0x045e: "Microsoft",
	0x046d: "Logitech",
	0x0480: "Toshiba",
	0x048d: "Integrated Technology Express",
	0x04b3: "IBM",
	0x04ca: "Lite-On Technology",
	0x04e8: "Samsung Electronics",
	0x04f2: "Chicony Electronics",
	0x04f3: "ELAN Microelectronics",
	0x050d: "Belkin International",
	0x056a: "Wacom",
	0x058f: "Alcor Micro",
	0x05ac: "Apple",
	0x0781: "SanDisk",
	0x0846: "NETGEAR",
	0x090c: "Silicon Motion",
	0x0930: "Toshiba",
	0x0951: "Kingston Technology",
	0x0b05: "ASUSTeK Computer",
	0x0b95: "ASIX Electronics",
	0x0bda: "Realtek Semiconductor",
	0x0bc2: "Seagate Technology",
	0x0c45: "Sonix Technology",
	0x0cf3: "Qualcomm Atheros",
	0x0df6: "Sitecom",
	0x0e8d: "MediaTek",
	0x1038: "SteelSeries",
	0x1058: "Western Digital",
	0x1199: "Sierra Wireless",
	0x125f: "ADATA Technology",
	0x12d1: "Huawei Technologies",
	0x13d3: "IMC Networks",
	0x13fd: "Initio",
	0x13fe: "Kingston Technology",
	0x148f: "Ralink Technology",
	0x152d: "JMicron Technology",
	0x1532: "Razer",
	0x154b: "PNY Technologies",
	0x174c: "ASMedia Technology",
	0x17ef: "Lenovo",
	0x18a5: "Verbatim",
	0x18d1: "Google",
	0x1a40: "Terminus Technology",
	0x1b1c: "Corsair",
	0x1bcf: "Sunplus Innovation Technology",
	0x1d6b: "Linux Foundation",
	0x1f75: "Innostor Technology",
	0x2001: "D-Link",
	0x2109: "VIA Labs",
	0x2188: "CalDigit",
	0x2717: "Xiaomi",
	0x291a: "Anker",
	0x2c7c: "Quectel Wireless Solutions",
	0x413c: "Dell",
	0x5986: "Bison Electronics",
	0x8564: "Transcend Information",
	0x8087: "Intel",
}

// pciClasses maps the PCI base class to a stable slug understood by the web UI.
var pciClasses = map[uint64]string{
	0x00: "other",
	0x01: "storage",
	0x02: "network",
	0x03: "display",
	0x04: "multimedia",
	0x05: "memory",
	0x06: "bridge",
	0x07: "communication",
	0x08: "system",
	0x09: "input",
	0x0a: "other",
	0x0b: "processor",
	0x0c: "usb",
	0x0d: "wireless",
	0x0e: "other",
	0x0f: "other",
	0x10: "encryption",
	0x11: "other",
	0x12: "processor",
	0x13: "other",
	0x40: "processor",
}

// usbClasses maps the USB interface class to a stable slug understood by the web UI.
var usbClasses = map[uint64]string{
	0x01: "audio",
	0x02: "communication",
	0x03: "input",
	0x05: "other",
	0x06: "video",
	0x07: "printer",
	0x08: "storage",
	0x09: "hub",
	0x0a: "communication",
	0x0b: "other",
	0x0d: "other",
	0x0e: "video",
	0x0f: "other",
	0xdc: "other",
	0xe0: "wireless",
	0xef: "other",
	0xfe: "other",
	0xff: "other",
}

func hexID(text string) (uint64, bool) {
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(trimHex(text), 16, 32)
	if err != nil {
		return 0, false
	}
	return value, true
}

func trimHex(text string) string {
	if len(text) > 2 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
		return text[2:]
	}
	return text
}

// hexPair renders an identifier the way lspci/lsusb do, for lookups by hand.
func hexPair(vendor, device uint64, known bool) string {
	if !known {
		return ""
	}
	const digits = "0123456789abcdef"
	render := func(value uint64) string {
		out := []byte{'0', '0', '0', '0'}
		for i := 3; i >= 0; i-- {
			out[i] = digits[value&0xf]
			value >>= 4
		}
		return string(out)
	}
	return render(vendor) + ":" + render(device)
}
