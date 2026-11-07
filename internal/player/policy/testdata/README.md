# Policy test fixtures

## Captured: a Mytek Brooklyn DAC+'s USB descriptors

`mytek-brooklyn-dac-plus.descriptors.bin` is the sysfs `descriptors`
attribute of a Mytek Brooklyn DAC+ (USB id `25ce:0025`, bcdDevice `0100`),
byte for byte as the owner's box read it on 2026-11-07 (B773): 970 bytes,
SHA-256 `02400f69dc719bfbc53b54fa74d990ee4bdce797be688942dd7be7c2f9eebece`.

It is the device descriptor (`bNumConfigurations` 2), then two
configurations of 476 bytes each, byte for byte the same, both headed
`0902dc01040100c001`: value 1. The kernel reads a configuration by its
index, so a unit that answers index 1 with index 0's bytes holds two copies
of one configuration. Its playback path is interface 2 (altset 1, S32_LE;
altset 2, native DSD), linked to input terminal 3, through feature unit 4
(a mute and a volume on the master channel) to output terminal 5, in the
audio function of control interface 0.

It holds no serial: the attribute carries no string descriptor (type 3),
and the device descriptor's `iSerialNumber` is an index (3), not the
string. `TestTheMyteksCapturedDescriptorsGiveItsDoPPath` checks both.
