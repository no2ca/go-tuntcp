## プロジェクトの概要
- Goでユーザー空間のTCPプロトコルスタックを作る
- AIを使いながら作っていくつもりだがある程度Goとかプロトコルについての理解をつけながら自力で進めていきたい
- だいたいこのマイルストーンで進める

## マイルストーン 
- [x] 1. TUN interface
- TUNからIP packetを読み書きする

- [x] 2. IPv4
- IPv4 headerのparse / serialize
- checksum計算

- [x] 3. TCP packet
- TCP headerのparse / serialize
- flags / options / checksum

- [ ] 4. TCP state machine
- `LISTEN`
- `SYN-SENT`
- `SYN-RECEIVED`
- `ESTABLISHED`
- `CLOSED`

- [ ] 5. 3-way handshake
- `SYN → SYN-ACK → ACK`
- TCP connectionを確立

- [ ] 6. データ送受信
- sequence number / ACK
- send / receive buffer
- payloadの送受信

- [ ] 7. Connection close
- `FIN / ACK`
- half-close
- `RST`

- [ ] 8. Retransmission
- timeout
- 再送
- retransmission queue

- [ ] 9. Flow control
- receive window
- advertised window
- zero window

- [ ] 10. TCP options
- MSS
- Window Scale
- SACK

- [ ] 11. Socket API
- `Listen`
- `Accept`
- `Connect`
- `Read`
- `Write`
- `Close`

- [ ] 12. 実環境で通信
- LinuxのTCP client ↔ 自作TCP server
- `curl`等で疎通確認

- [ ] 13. 発展
- congestion control
- IPv6
- SACKの本格対応
- 複数connection
- 性能測定・最適化
