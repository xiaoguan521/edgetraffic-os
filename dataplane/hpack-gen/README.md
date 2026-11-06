# hpack-gen

生成字符串 `syedge` 的 HPACK 哈夫曼编码字节（RFC 7541 附录 B），替换 HTTP/2 filter 中的 server 常量。
P0 实现为一个 Go 小程序，并附解码回读的单元测试。
