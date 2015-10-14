import * as CodeActions from "./CodeActions";
import CodeStore from "./CodeStore";
import Dispatcher from "./Dispatcher";
import defaultXhr from "xhr";

// TODO preloading
const CodeBackend = {
	xhr: defaultXhr,

	handle(action) {
		switch (action.constructor) {
		case CodeActions.WantFile:
			let file = CodeStore.files.get(action.repo, action.rev, action.tree);
			if (file === undefined) {
				CodeBackend.xhr({
					uri: `/ui/${action.repo}@${action.rev}/.tree/${action.tree}`,
					json: {},
				}, function(err, resp, body) {
					// TODO handle error
					Dispatcher.dispatch(new CodeActions.FileFetched(action.repo, action.rev, action.tree, body));
				});
			}
			break;
		}
	},
};

Dispatcher.register(CodeBackend.handle);

export default CodeBackend;
