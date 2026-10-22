# Embedded Chinese dictionary

`dict_zh.txt` is the Simplified Chinese word list used by `internal/search` for
word segmentation. Each line is `word frequency part-of-speech`, the jieba
dictionary format that [go-ego/gse](https://github.com/go-ego/gse) reads.

- Source: `go-ego/gse` v1.1.0, `data/dict/zh/s_1.txt`.
- License: gse is Apache-2.0; the dictionary originates from
  [fxsjy/jieba](https://github.com/fxsjy/jieba), which is MIT.
- This copy: the 100,000 most frequent entries of `s_1.txt`, sorted by the
  frequency column. The full list has 352,279 entries; the tail is long
  proper-noun lists that do not appear in general notes, and the search index
  keeps character and bigram terms as a fallback for unknown words.

Regenerate from a gse module cache:

```sh
awk '{print $2"\t"$0}' \
  "$(go env GOMODCACHE)/github.com/go-ego/gse@v1.1.0/data/dict/zh/s_1.txt" |
  sort -rn | head -100000 | cut -f2- > internal/search/data/dict_zh.txt
```

To use the full dictionary instead, copy `s_1.txt` to `dict_zh.txt`; the
segmentation code does not change, but the binary grows by about 3.6 MB and the
loaded dictionary uses about 115 MB of heap instead of about 32 MB.
