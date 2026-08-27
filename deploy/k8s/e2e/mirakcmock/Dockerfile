# mirakc モックのイメージ。
#
# **ビルドはこの Dockerfile の外で行う**（run.sh が `go build` して、バイナリと
# この Dockerfile だけを入れた一時ディレクトリを context にする）。マルチ
# ステージにして golang イメージを引くと、クラスタを立て直すたびに数百 MB を
# 引き直すことになり、ハーネスを回す敷居がその一点で上がる。
#
# `scratch` なのでシェルも curl も入っていない。モックの中を見たいときは
# `/mock/stats` を叩くか、`kubectl logs` を読む。
FROM scratch
COPY mirakcmock /mirakcmock
EXPOSE 40772
ENTRYPOINT ["/mirakcmock"]
