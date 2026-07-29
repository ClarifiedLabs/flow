# Vendored xterm.js assets

These browser-ready UMD bundles and the xterm.js stylesheet were copied without
modification from the npm tarballs listed below. The versions were resolved
from each package's `latest` dist-tag on 2026-07-28.

| Package | Version | Vendored bundle | UMD global | npm tarball integrity |
| --- | --- | --- | --- | --- |
| [`xterm`](https://www.npmjs.com/package/xterm) | 5.3.0 | `xterm.js` (`package/lib/xterm.js`), `xterm.css` (`package/css/xterm.css`) | `Terminal` | `sha512-8QqjlekLUFTrU6x7xck1MsPzPA571K5zNqWm0M0oroYEWVOptZ0+ubQSkQ3uxIEhcIHRujJy6emDWX4A7qyFzg==` |
| [`@xterm/addon-fit`](https://www.npmjs.com/package/@xterm/addon-fit) | 0.11.0 | `xterm-addon-fit.js` (`package/lib/addon-fit.js`) | `FitAddon` (`FitAddon.FitAddon`) | `sha512-jYcgT6xtVYhnhgxh3QgYDnnNMYTcf8ElbxxFzX0IZo+vabQqSPAjC3c1wJrKB5E19VwQei89QCiZZP86DCPF7g==` |
| [`@xterm/addon-web-links`](https://www.npmjs.com/package/@xterm/addon-web-links) | 0.12.0 | `xterm-addon-web-links.js` (`package/lib/addon-web-links.js`) | `WebLinksAddon` (`WebLinksAddon.WebLinksAddon`) | `sha512-4Smom3RPyVp7ZMYOYDoC/9eGJJJqYhnPLGGqJ6wOBfB8VxPViJNSKdgRYb8NpaM6YSelEKbA2SStD7lGyqaobw==` |

Source tarballs:

- <https://registry.npmjs.org/xterm/-/xterm-5.3.0.tgz>
- <https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz>
- <https://registry.npmjs.org/@xterm/addon-web-links/-/addon-web-links-0.12.0.tgz>

Each downloaded tarball was hashed with SHA-512, encoded as base64, prefixed
with `sha512-`, and compared byte-for-byte with the `dist.integrity` value in
npm registry metadata before its bundle was extracted. The UMD wrappers were
also inspected to confirm the globals shown above.
