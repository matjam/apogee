# Changelog

## [1.1.0](https://github.com/matjam/apogee/compare/v1.0.0...v1.1.0) (2026-09-26)


### Features

* **jit:** compile math.floor, ceil, abs, min and max inline ([#138](https://github.com/matjam/apogee/issues/138)) ([6304bb3](https://github.com/matjam/apogee/commit/6304bb3ee41509acbe53079fa0e063bd8c614d42))


### Performance Improvements

* **jit:** % of floats by a power of two in compiled code ([#137](https://github.com/matjam/apogee/issues/137)) ([593fa2b](https://github.com/matjam/apogee/commit/593fa2b5def47560c6afff782a62c4a3d165b99d))
* **jit:** bitwise operators and // of floats in kernels ([#135](https://github.com/matjam/apogee/issues/135)) ([471af65](https://github.com/matjam/apogee/commit/471af657784162134b969a99eec7fce87f0dc554))
* **jit:** kernel intrinsic arguments may read buffers ([#132](https://github.com/matjam/apogee/issues/132)) ([065eef5](https://github.com/matjam/apogee/commit/065eef5364228137d336ba83bdf3135e0c89e0a1))
* **jit:** kernels index buffers with integral float keys ([#136](https://github.com/matjam/apogee/issues/136)) ([d353fa8](https://github.com/matjam/apogee/commit/d353fa8c3db3ed38ea4d0031b92caec9edc5faa7))
* **jit:** kernels load integer constants as floats where no use can tell ([#133](https://github.com/matjam/apogee/issues/133)) ([499f69c](https://github.com/matjam/apogee/commit/499f69c40f4b64e6bd7baa1046cd921950eb8a62))
* **jit:** kernels read numbers and buffers from upvalues ([#128](https://github.com/matjam/apogee/issues/128)) ([aaeb167](https://github.com/matjam/apogee/commit/aaeb167bffb4930c22ca2f02de5019ce72ffafac))
* **jit:** math.floor, ceil, abs, min and max in kernels ([#139](https://github.com/matjam/apogee/issues/139)) ([c7b872f](https://github.com/matjam/apogee/commit/c7b872f4fb1136b2b1b73a1ea0d30370fb927774))
