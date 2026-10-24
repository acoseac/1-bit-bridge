# OpenHome service descriptions (upstream, pinned, test data only)

These are OpenHome's own service descriptions for the four services the
bridge's player carries, from
[github.com/openhome/ohNetGenerated](https://github.com/openhome/ohNetGenerated)
at commit `22feef5262ed28432783c9bf16bdeb31d4c8bb18`, path
`OpenHome/Net/Service/Upnp/OpenHome/`, fetched 2026-10-23 with the owner's
approval. They are under the MIT licence in `License.txt`, from the same
commit. They are unmodified: Playlist1.xml and License.txt keep their CRLF
line endings, which `.gitattributes` keeps from any conversion.

They are test data and nothing else. The bridge's SCPDs are its own,
generated from its tables (`internal/player/openhome/services.go`), and
`TestOurSCPDsAreUpstreamsInventory` compares them with these files exactly:
every action, its arguments in order with their directions and related
state variables, and every state variable's type, allowed values and
eventing. No file here is built into a binary, so no third-party notice
ships. `TestTheUpstreamFilesAreTheirPinnedBytes` holds each file to its
hash below.

```
c05eeb4de8c5cde13f5fee4c5e0a81686fce9897d43776a91da4413cd2eed058  Product1.xml
a92bd0d6e44a22856a1c55ab23cec7bb7f807542ddfe5d70f379f70fc6c07dba  Playlist1.xml
15e31ab3d62b9f6cb7a5c6a61b519a097b95c31ffe822ffec2197cde9380242b  Info1.xml
1ca6e132de26298a5a083d1108bfdfa2c97ed7273c24354b003dd4d31c7b2984  Time1.xml
752f71972151a4e06d287898c709c8a81bccb487d3d23cff082b681eb2ec8ce3  License.txt
```
