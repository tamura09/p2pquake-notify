# ビルドは常にランナーのネイティブアーキテクチャで行い、GOARCH だけを arm64 に
# します。最終段を --platform=linux/arm64 で組んでもコンパイルはここで済んでいるので、
# QEMU エミュレーションは一切走りません(エミュレーション下のGoビルドは数分かかります)。
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src

# 依存の取得を先に済ませ、ソースだけ変わった時にこの層を再利用します。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO を切って完全な静的バイナリにします。distroless の static イメージには
# libc が無いので、動的リンクだと起動できません。
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="-s -w" -o /out/p2pquake-notify .

# static-debian12 はシェルもパッケージマネージャも持たず、CA証明書だけを持ちます。
# タイムゾーンデータは time/tzdata でバイナリに埋め込み済みです。
FROM --platform=linux/arm64 gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/p2pquake-notify /p2pquake-notify

USER nonroot:nonroot
ENTRYPOINT ["/p2pquake-notify"]
