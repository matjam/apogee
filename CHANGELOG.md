# Changelog

## [1.1.1](https://github.com/matjam/apogee/compare/v1.1.0...v1.1.1) (2026-09-27)


### Bug Fixes

* **jit:** a tail call from a vararg function keeps its frame where Go sees it ([#152](https://github.com/matjam/apogee/issues/152)) ([a8145a9](https://github.com/matjam/apogee/commit/a8145a9f4498059de82df6a7a887ebbe980223a4))
* **jit:** an integer constant passed as a call's later argument stays an integer ([#141](https://github.com/matjam/apogee/issues/141)) ([f226721](https://github.com/matjam/apogee/commit/f226721ece59915673558357cb70bbe5e0417058))
* **lua:** absent __index no longer hides __len and __eq ([#145](https://github.com/matjam/apogee/issues/145)) ([4850b8d](https://github.com/matjam/apogee/commit/4850b8d6b074a9e40cb284da85e267c0b3516174))


### Performance Improvements

* **jit:** a kernel's loop variable shares the index's register ([#155](https://github.com/matjam/apogee/issues/155)) ([42f78f1](https://github.com/matjam/apogee/commit/42f78f145decb8ab099dd0dcf62e005355e5d8cf))
* **jit:** compile # of tables and buffers, and == of a float and an integer ([#146](https://github.com/matjam/apogee/issues/146)) ([393344a](https://github.com/matjam/apogee/commit/393344a7908983ec54002cee8a7e8028445d4c6f))
* **jit:** compile % of floats by any divisor ([#148](https://github.com/matjam/apogee/issues/148)) ([3341cbe](https://github.com/matjam/apogee/commit/3341cbe16da1182ff98829cd89396cf2060bf194))
* **jit:** compile calls and returns of any number of values ([#143](https://github.com/matjam/apogee/issues/143)) ([b2173b0](https://github.com/matjam/apogee/commit/b2173b052120f8c97307446f304e7f87a94d83b7))
* **jit:** compile calls and tail calls of vararg functions, and VARARG ([#156](https://github.com/matjam/apogee/issues/156)) ([45b42e8](https://github.com/matjam/apogee/commit/45b42e8f3464e6bd7077535cdeec6801dd4d4e71))
* **jit:** compile generic for over ipairs and pairs ([#149](https://github.com/matjam/apogee/issues/149)) ([656b40f](https://github.com/matjam/apogee/commit/656b40f167f4ebbbeca0469956ccd284c0750607))
* **jit:** compile math.min and max of a float and an integer ([#147](https://github.com/matjam/apogee/issues/147)) ([036361d](https://github.com/matjam/apogee/commit/036361dec6b09a0c440a57d67ab8fe1ba65dd184))
* **jit:** compile setmetatable of a new table, called or tail called ([#154](https://github.com/matjam/apogee/issues/154)) ([b23e5e3](https://github.com/matjam/apogee/commit/b23e5e3ea2078fbf0500639156253e136f55098f))
* **jit:** keep a Lua call's code together ([#150](https://github.com/matjam/apogee/issues/150)) ([7297edd](https://github.com/matjam/apogee/commit/7297edd9984d4f83066b227b761032ab51f7acb0))
* **jit:** store nil into array elements in compiled code ([#151](https://github.com/matjam/apogee/issues/151)) ([ec5224c](https://github.com/matjam/apogee/commit/ec5224c0a60ddc4e89c23c7261f78cef76bf3477))

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
