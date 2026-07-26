# Maintainer: J. Binder <j.binder@zoho.com>
pkgname=smsd
pkgver=0.1.0
pkgrel=1
pkgdesc="Native Linux tray daemon that imports Android SMS over adb into local SQLite with a read-only viewer"
arch=('x86_64' 'aarch64')
url="https://github.com/jbinder/smsd"
license=('MIT')
# Runtime deps: adb for the device link, libnotify for desktop notifications,
# and a StatusNotifierItem host for the tray (any of the appindicator/SNI
# implementations used by polybar, waybar, etc.).
depends=('android-tools' 'libnotify')
optdepends=('xdg-utils: open the conversation viewer in a browser')
makedepends=('go>=1.24')
# SQLite is provided by the pure-Go driver compiled into the binary — no
# libsqlite3 runtime dependency.
source=("$pkgname-$pkgver.tar.gz::$url/archive/refs/tags/v$pkgver.tar.gz")
sha256sums=('SKIP')

build() {
  cd "$pkgname-$pkgver"
  export CGO_ENABLED=0
  export GOFLAGS="-trimpath -mod=readonly -modcacherw"
  export GOPATH="$srcdir/gopath"
  go build -ldflags "-s -w -X main.version=$pkgver" -o "$pkgname" ./cmd/smsd
}

check() {
  cd "$pkgname-$pkgver"
  export CGO_ENABLED=0
  go test ./...
}

package() {
  cd "$pkgname-$pkgver"
  install -Dm755 "$pkgname" "$pkgdir/usr/bin/$pkgname"
  install -Dm644 systemd/smsd.service "$pkgdir/usr/lib/systemd/user/smsd.service"
  install -Dm644 README.md "$pkgdir/usr/share/doc/$pkgname/README.md"
  install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
}
