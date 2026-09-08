package providerplugin

func CanonicalMigratedProviderPreset(providerID string) (string, string, string, bool) {
	switch providerID {
	case "aihubmix":
		return "crux.catwalk.aihubmix", "0.51.23", "ae5374ffce2ca8951c7e5bbd9d466e716d380dafb083c2dda53384a9adff23ce", true
	case "alibaba-singapore":
		return "crux.catwalk.alibaba-singapore", "0.51.23", "70bec943cbce39e28bad6faa57d5b9533ff425fc6cc6028ecb406dafc374b3aa", true
	case "alibaba-us":
		return "crux.catwalk.alibaba-us", "0.51.23", "501bcce05acfeca8485cfee1939ba13bbc5cc6ac9b4e1b89555411c545e7b8aa", true
	case "atlascloud":
		return "crux.catwalk.atlascloud", "0.51.23", "f73aa0b913a1564e5ee65731c7e5a4d312c145eb631ea07fd99d827c2cf5abca", true
	case "avian":
		return "crux.catwalk.avian", "0.51.23", "f23e78a326d9321376291cb9cd2ad9e06dd359be0a233468f02673d15a1d859e", true
	case "baseten":
		return "crux.catwalk.baseten", "0.51.23", "b9520c2a2be4dd3499fda0757ac8d1fdf01925a5c1deb6483bcc653ace4d1111", true
	case "cerebras":
		return "crux.catwalk.cerebras", "0.51.23", "b8e331ce3268b48efdb9b02b96c4d0c2e16ba2198b52bdd6325d275691e74809", true
	case "chutes":
		return "crux.catwalk.chutes", "0.51.23", "df0a0788883f2987c8550def78d4b92e7eb12c515639db9476c900c0851ac5a6", true
	case "deepseek":
		return "crux.catwalk.deepseek", "0.51.23", "db64f6257fef73b1325bdc2bc6203e6b0c8777ac4b3f6ea4996bdae3f0d6d719", true
	case "fireworks":
		return "crux.catwalk.fireworks", "0.51.23", "cfebd7366c0ee43dda35d7d63683811501633e4cbe620aff25c2d5fac7bd3f0b", true
	case "groq":
		return "crux.catwalk.groq", "0.51.23", "dffd305fc5f5dc5973f35582a06dd18aba04a8f2b64a1ed8eb247132ee519145", true
	case "huggingface":
		return "crux.catwalk.huggingface", "0.51.23", "9baa3e9941c2cce5e9d7144f5aa1fe1d1cab68608c3ebf804f2d185f5af1082a", true
	case "ionet":
		return "crux.catwalk.ionet", "0.51.23", "ab1a07c5fe240c04b02bb47d8729f42b6effdeab33c2edda87635cdff25037f9", true
	case "moonshot":
		return "crux.catwalk.moonshot", "0.51.23", "b63c055ac58e32cb9d333a237920cb8ab51462ce474d4760284f9661c15d6f16", true
	case "nebius":
		return "crux.catwalk.nebius", "0.51.23", "75c93871c775c2c5ec0e264157c41445692b798466884980907b16b8b2352b39", true
	case "neuralwatt":
		return "crux.catwalk.neuralwatt", "0.51.23", "da683c247254d7e852323be345492ab5d02ab44cca95310aa5f4dd48f34e663b", true
	case "opencode-go":
		return "crux.catwalk.opencode-go", "0.51.23", "e42ec06e1ec1a4c7449b4de9d7694f0588947bbf9c838cd8e8186fc0054a2780", true
	case "opencode-zen":
		return "crux.catwalk.opencode-zen", "0.51.23", "fc06512f14621c910a9189723b9c1db2a9a18454be941ef53d630204079a756f", true
	case "qiniucloud":
		return "crux.catwalk.qiniucloud", "0.51.23", "89a1f299c996000ba6bc0678793551aed260d6084300443c4c1049dc34285884", true
	case "scaleway":
		return "crux.catwalk.scaleway", "0.51.23", "9b5058aa8c42b2a9d8d0fd8bd5e7f56105144b825848f4268a7a7acc4ffdfbbe", true
	case "synthetic":
		return "crux.catwalk.synthetic", "0.51.23", "544db7ccd9d11e3dfedf3a7079998e590f8534c4f02f1ee34e1383e471219236", true
	case "venice":
		return "crux.catwalk.venice", "0.51.23", "5ab1299a08c1deded7a9d8d167fa74de09ea92cbe8a8501d0f08fd3496235356", true
	case "xai":
		return "crux.catwalk.xai", "0.51.23", "ddebc16179a4e8b5c595019a1919e813aef60bd83b383ef64c1365c828472772", true
	case "zai":
		return "crux.catwalk.zai", "0.51.23", "a515113fef14724ea9dd309eaae17ef5c85db2bfcec02a0a5fdb0804c82edb4e", true
	case "zhipu":
		return "crux.catwalk.zhipu", "0.51.23", "2687a57ce4e801f39fc3dbd18e6af8c87cc485e09b7702b78548644491afa26b", true
	case "zhipu-coding":
		return "crux.catwalk.zhipu-coding", "0.51.23", "ec87fd33d1814458aef808f7018c41502d3071af1cc26fe522248c117da4a597", true
	default:
		return "", "", "", false
	}
}

func MigratedProviderPreset(providerID string) (string, string, bool) {
	id, version, _, migrated := CanonicalMigratedProviderPreset(providerID)
	return id, version, migrated
}
