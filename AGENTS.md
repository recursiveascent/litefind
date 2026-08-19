* Use `jj` for source code repository operations.
* Current Go version 1.26.3 - minimum Go 1.26 version for the module.
* Only Go stdlib packages, unless explicitly authorized.
* Use `go doc` and `gopls` when reading, debugging, exploring, and understanding Go code.
* If something doesn't work, propagate the error or exit or crash. Do not have "fallbacks".
* Do not keep old methods around for "compatibility"; this is a new project and there are no compatibility concerns yet.
* Never add sleeps to tests.
* Use `twee`, if available, to drive and inspect interactive terminal sessions while developing or debugging, especially terminal state; prefer `wait stable`/`wait text` and traces over timing guesses.
* Brevity, brevity, brevity! Do not do weird defaults; have only one way of doing things; refactor relentlessly as necessary.
* Commit your changes before finishing your turn.
* Err on the side of not exporting symbols from Go packages, only what is minimally necessary. Don't let this module's API become an accidental support burden.
