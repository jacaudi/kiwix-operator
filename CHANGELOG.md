# Changelog

## 1.0.0 (2026-11-27)


### Features

* **api:** KiwixLibrary and ZimArchive v1alpha1 types with CEL validation ([af3244c](https://github.com/jacaudi/kiwix-operator/commit/af3244c0dfd09d9dda3b59bfc1bc18c92abb5e09))
* **catalog:** kiwix.org catalog lookup and source fingerprints ([d640d12](https://github.com/jacaudi/kiwix-operator/commit/d640d12d71669ee7f229aa3a639ee91c262de89d))
* **controller:** KiwixLibrary reconciler ([2add803](https://github.com/jacaudi/kiwix-operator/commit/2add803ce50c4992848ec4a403d682753df87aed))
* **controller:** ZimArchive gating, promotion, notifications and finalizer ([3a0f617](https://github.com/jacaudi/kiwix-operator/commit/3a0f6173b0f18cd06befcf20abd0b2c47e141d52))
* **controller:** ZimArchive status mirror, adoption, due evaluation and attempt start ([cbdfece](https://github.com/jacaudi/kiwix-operator/commit/cbdfecee9796ec9719dae100bbc424ff8f25d8e2))
* **gate:** pre-checks, health gate and due rules ([7abd469](https://github.com/jacaudi/kiwix-operator/commit/7abd469af9c695b6febcd8687d77d1ccdc655623))
* **libclient:** typed librarian API client ([6de4fea](https://github.com/jacaudi/kiwix-operator/commit/6de4fea70baeef6a8d0c6f99b53f6cccee2bff9a))
* **librarian:** attempt state machine, part uploads and disk reserve ([7caf6e7](https://github.com/jacaudi/kiwix-operator/commit/7caf6e7566e89a65192498df63be64543b4bae17))
* **librarian:** compare-and-swap promotion, adoption and deletion ([badc32e](https://github.com/jacaudi/kiwix-operator/commit/badc32e50b5236621f1994a5e459ad8ef30809e4))
* **librarian:** HTTP API, startup order and librarian subcommand ([a0e0d7b](https://github.com/jacaudi/kiwix-operator/commit/a0e0d7bf1b881f4208fe4b7195a5027d8b11daed))
* **librarian:** manifest, atomic writes and volume scan ([f8e9b9b](https://github.com/jacaudi/kiwix-operator/commit/f8e9b9b178aa641f3d5e86628df6c4fdafd967d9))
* **librarian:** read book metadata with kiwix-manage ([f5d7f09](https://github.com/jacaudi/kiwix-operator/commit/f5d7f090291d4bcec05c41370f0dc33349ccd01f))
* **librarian:** render library.xml with confirmed writes ([a3a4f2b](https://github.com/jacaudi/kiwix-operator/commit/a3a4f2b88e02d500ae7da4e77d855db907583b99))
* **librarian:** resumable verified fetches ([b7f59c5](https://github.com/jacaudi/kiwix-operator/commit/b7f59c56af4e3207382ab9cebbc9f0bc1f46d737))
* **manager:** manager wiring, RBAC and Helm chart ([a92ec3c](https://github.com/jacaudi/kiwix-operator/commit/a92ec3c5322d4a357175a19c264b6209f3ec88ca))
* **notify:** webhook notifications ([e1d9a3b](https://github.com/jacaudi/kiwix-operator/commit/e1d9a3bf742c17cf90bb118d4d6d476fd4916ab0))
* **operator:** kiwix-operator v1alpha1 ([983d49b](https://github.com/jacaudi/kiwix-operator/commit/983d49b63f148b2813bd93708f969706901fc1dd))
* **upload:** stream finished crawls to the librarian ([3809546](https://github.com/jacaudi/kiwix-operator/commit/3809546bb407e0debc1f7e9406f962c886c03f0c))
* **zim:** header UUID, name rules and test fixtures ([f7bddc8](https://github.com/jacaudi/kiwix-operator/commit/f7bddc849bfb89b72d70e67edf2d2ba6b97de643))


### Bug Fixes

* address security audit finding S1 ([98c1a26](https://github.com/jacaudi/kiwix-operator/commit/98c1a261ab857a3735458091abde95d207629441))
* address security audit finding S2 ([dc7010b](https://github.com/jacaudi/kiwix-operator/commit/dc7010b46e11531ae34056127341fa57d30e08fc))
* address security audit finding S3 ([3665c8b](https://github.com/jacaudi/kiwix-operator/commit/3665c8ba5763ebeb530507b1f7ec4fde0adb511a))
* address security audit finding S4 ([15f7480](https://github.com/jacaudi/kiwix-operator/commit/15f74808c173c9656df2908a74443c9b3cdf769b))
* address security audit finding S5 ([851763b](https://github.com/jacaudi/kiwix-operator/commit/851763b129bbc3af0762fba1e12bf2cbbbcfe12e))
* address security audit finding S6 ([483542b](https://github.com/jacaudi/kiwix-operator/commit/483542b4fb63393be2809dafda155622b1d0811a))
* address security audit finding S6 ([16409ec](https://github.com/jacaudi/kiwix-operator/commit/16409ec326c9a1e2ec99a304c0cf800c86b9580e))
* **api:** bound the crawl timeout so it always decodes ([0e79252](https://github.com/jacaudi/kiwix-operator/commit/0e79252cff9b15f6db7d6f9229aae63db3da5745))
* **api:** omit an empty storageClassName and reject empty claim, url and libraryRef names ([8a3c88d](https://github.com/jacaudi/kiwix-operator/commit/8a3c88d02c6fdf81db1288fc4b7c31524bcebd56))
* **api:** require spec and validate crawl timeout format ([2d16140](https://github.com/jacaudi/kiwix-operator/commit/2d16140be91a808b40c586b1a75be9db5213d82f))
* **catalog:** enforce the 30 s request timeout for injected HTTP clients ([f63804a](https://github.com/jacaudi/kiwix-operator/commit/f63804afb49ccff30097557041f29388e251a638))
* **chart:** use a catalog flavour that exists in the sample and NOTES ([ef8ae40](https://github.com/jacaudi/kiwix-operator/commit/ef8ae40a564f34e115507f19e93db77b8434051f))
* **chart:** v-prefixed image tags, bare chart versions ([8ee0aaf](https://github.com/jacaudi/kiwix-operator/commit/8ee0aaf3a6bd6fbc2050a982cd09866f1ec5e3b9))
* **ci:** publish and verify the bare chart image tag, document fork PRs ([5ab1327](https://github.com/jacaudi/kiwix-operator/commit/5ab132773d2e9db8d85ad18d80551876c458c53a))
* **controller:** bound failure messages where they are captured ([e76dd01](https://github.com/jacaudi/kiwix-operator/commit/e76dd0152c439cb5ec45946c2cb153974256d947))
* **controller:** carry the source fingerprint in an adoption ([170edf2](https://github.com/jacaudi/kiwix-operator/commit/170edf255132ecaf936c2e06183f2cff05332b82))
* **controller:** fail an open attempt whose source kind changed ([12c2538](https://github.com/jacaudi/kiwix-operator/commit/12c2538c19dccc90d856ee63f2d684c074ad6a91))
* **controller:** keep the stored condition message when the cause is unchanged ([5f29a52](https://github.com/jacaudi/kiwix-operator/commit/5f29a5230a7a8ecda2cbdf79f27a9e2ca302f8b2))
* **controller:** record the discard outcome first and confirm a missing crawl Job ([af6da4b](https://github.com/jacaudi/kiwix-operator/commit/af6da4bb7edaf34ac54c8f33cae0c476dbb131d8))
* **controller:** refresh an error condition's message after a spec edit ([c0ffd05](https://github.com/jacaudi/kiwix-operator/commit/c0ffd0540ae4246aaeb9734d8936f85685677ba0))
* **controller:** set AwaitingAdoption in dueInput ([bd3afc0](https://github.com/jacaudi/kiwix-operator/commit/bd3afc0b52b477be4ef4664c91b35c6d4432a6f1))
* **controller:** stop the status-write reconcile loop and queue before the catalog lookup ([798e031](https://github.com/jacaudi/kiwix-operator/commit/798e031b30cfd5df70e7ee83084825abc6ab5521))
* **e2e:** pin the e2e kubeconfig everywhere and fix the green-run failures ([53840d6](https://github.com/jacaudi/kiwix-operator/commit/53840d6d4cc4fd961c5cf1d9e87d19e41ceb00a1))
* **librarian:** answer busy, not conflict, while a part body is in flight ([1b16eb5](https://github.com/jacaudi/kiwix-operator/commit/1b16eb59b49afa1f3abb13f9b03709bdb70fb765))
* **librarian:** bound each confirm call by the write window ([0ea23cb](https://github.com/jacaudi/kiwix-operator/commit/0ea23cb26c2cca17ec45f9e0a10ac95b45e46cdb))
* **librarian:** bound kiwix-manage with a deadline and kill grace ([a4b3c3d](https://github.com/jacaudi/kiwix-operator/commit/a4b3c3d527b9f7886671767dafc72823c5415c8a))
* **librarian:** drop an unused nolint directive from the kiwix-manage test ([59ecd83](https://github.com/jacaudi/kiwix-operator/commit/59ecd832c68dbb5ae8f354532000dfdeef1499b4))
* **librarian:** fsync the data directory before forgetting an unlinked file ([77e12e5](https://github.com/jacaudi/kiwix-operator/commit/77e12e5e842e7ace6b581d5f138c1fabd5c4f520))
* **librarian:** hold the reserve and the declared size on fetches ([b1c8fc9](https://github.com/jacaudi/kiwix-operator/commit/b1c8fc9548edeae21e9da54e6c1cc809d07f2c8f))
* **librarian:** keep a promotion's WARCs on roll-forward ([c6286c6](https://github.com/jacaudi/kiwix-operator/commit/c6286c6a816549c647329a62ba33d3ac855e3c9f))
* **librarian:** keep roll-forward going past a failing intent ([55bd7c9](https://github.com/jacaudi/kiwix-operator/commit/55bd7c9bba71b8dd3ba4c1d02d614ebc827e5e03))
* **librarian:** keep upload parts in the manifest and let canonical files win duplicates ([f641189](https://github.com/jacaudi/kiwix-operator/commit/f6411899f2342c088e142dd5e1cf53958f01746d))
* **librarian:** make the reserve check overflow-safe ([5e311b8](https://github.com/jacaudi/kiwix-operator/commit/5e311b86ea87a3aa862c91f271f6bd3feb9662bf))
* **librarian:** reject a part PUT without a Content-Length header ([5f92d2e](https://github.com/jacaudi/kiwix-operator/commit/5f92d2ea2805ffc320295a5acb27d06a0a16a808))
* **librarian:** report InsufficientSpace for a crawl upload refused for space ([357c325](https://github.com/jacaudi/kiwix-operator/commit/357c3250574863ae8df11b40828dd1189ac788c5))
* **repo:** run the manager subcommand from task run ([b5b20d0](https://github.com/jacaudi/kiwix-operator/commit/b5b20d0e56468a270080cb28be0574b70fb0388d))
* **test:** always run the e2e uncached ([49f0403](https://github.com/jacaudi/kiwix-operator/commit/49f04033fb15353a9197ce0b2215d512312efb70))
* **upload:** share one backoff budget across the run ([8ff30c9](https://github.com/jacaudi/kiwix-operator/commit/8ff30c96c74fd84a4354927be5de211b296246d7))


### Refactors

* **api:** delete the unused Adopting and CurrentChanged reasons ([4360ee9](https://github.com/jacaudi/kiwix-operator/commit/4360ee97a4e8b230653fe4c9eea62c9bc7a94dff))
