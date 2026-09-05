package providerplugin

func CanonicalMigratedProviderPreset(providerID string) (string, string, string, bool) {
	switch providerID {
	case "aihubmix":
		return "crux.catwalk.aihubmix", "0.51.23", "c9d17fafb3b9fe3662eaffffa5ce79a781871ed2e60c27e06396594ced0d0a9b", true
	case "alibaba-singapore":
		return "crux.catwalk.alibaba-singapore", "0.51.23", "921da2709b012e7b4df36b57462ed1d7df2a93ecc16904da10a9421e8d6bf939", true
	case "alibaba-us":
		return "crux.catwalk.alibaba-us", "0.51.23", "81c90f288436312e54f30c196565f43354b3524fed12114678d178bf8bd3dbe3", true
	case "atlascloud":
		return "crux.catwalk.atlascloud", "0.51.23", "272092db0ad960cfa6eb0f22fdc17c58840576da478463b7ac7ba05668d1ea95", true
	case "avian":
		return "crux.catwalk.avian", "0.51.23", "26f96c10b4a8a272d2fcf09260e3f8b7101c30964337eca714a2a4d6319b7888", true
	case "baseten":
		return "crux.catwalk.baseten", "0.51.23", "40513645839ed8cbccde8489aa54a126358761055f0d59d3bf1216f7eadf8c86", true
	case "cerebras":
		return "crux.catwalk.cerebras", "0.51.23", "a81fd6ee1374e51fd8faa7947d4c7aa06161dca19b82daf1916e5bf892a8deee", true
	case "chutes":
		return "crux.catwalk.chutes", "0.51.23", "34743a1ebf43bc4d6e07b6241ff46226513de9cc2cd20ecdfee7f8f91ae100ca", true
	case "deepseek":
		return "crux.catwalk.deepseek", "0.51.23", "124ed6519d2a2d7df8e45aef949aedc8da3d850e2cad25dc41efce7a0ea4176b", true
	case "fireworks":
		return "crux.catwalk.fireworks", "0.51.23", "d905aae787584d1fc22f40f7ed18bccc223a8a512f52e9cab86903292da8c907", true
	case "groq":
		return "crux.catwalk.groq", "0.51.23", "3ea879603a815962e8f6f78b49bef993a6eef235658983ec5efa880c8df3d637", true
	case "huggingface":
		return "crux.catwalk.huggingface", "0.51.23", "2a16f4a865a659831c9c9aec41c992d8a443e16ee5ae6ad74f06b87525ce1efe", true
	case "ionet":
		return "crux.catwalk.ionet", "0.51.23", "d21cad49a31318094a2a62ea7aeb26f11fbbb62c3d3a5d03d5c19e0628488475", true
	case "moonshot":
		return "crux.catwalk.moonshot", "0.51.23", "b57b1049df6b50a880ad4de389403712a4c9cde72c2dc2b87a64530ead8b1bd7", true
	case "nebius":
		return "crux.catwalk.nebius", "0.51.23", "78d25eee456cc0855a07306b7b027eaef2f8d7ccc62f2dc47b2d3ce57bd6cafe", true
	case "neuralwatt":
		return "crux.catwalk.neuralwatt", "0.51.23", "9e7f27eef64955e1e543369af174c41e034898b68038f9a9dc838b299f1fae0e", true
	case "opencode-go":
		return "crux.catwalk.opencode-go", "0.51.23", "38f2108ed6ea372fc1091d36e332ca634e293dd5d4d5d193283c27a9bdf64e67", true
	case "opencode-zen":
		return "crux.catwalk.opencode-zen", "0.51.23", "8fee36eb466e13d3b3a027516d1413f869d131f0df75e81c51b868b5b8df3562", true
	case "qiniucloud":
		return "crux.catwalk.qiniucloud", "0.51.23", "51865d24a52f99c27dabbf3c82347f6464c90589ce94ccee975941decc1a77e9", true
	case "scaleway":
		return "crux.catwalk.scaleway", "0.51.23", "c8efb69fc6cf1fce48639ade6e3505541130cbc33aeb20b6bf1cd50fcc4d5dda", true
	case "synthetic":
		return "crux.catwalk.synthetic", "0.51.23", "e8294d0645240ac3b8c7421b024537ea390b52b02c2f5260a67d2aadbb5c7fbf", true
	case "venice":
		return "crux.catwalk.venice", "0.51.23", "cc117e682f39440d937f53c7346be0730f7fdfc0e348266362b4753e1ead4502", true
	case "xai":
		return "crux.catwalk.xai", "0.51.23", "97e9a637fa2c64c347c185ab90958c0249ee3555bec710fbfdae26387c80b682", true
	case "zai":
		return "crux.catwalk.zai", "0.51.23", "f54951df7c80faa353bf9b328ccc72caf8ef9f2625c1792defe57786b1c2b588", true
	case "zhipu":
		return "crux.catwalk.zhipu", "0.51.23", "f52347b4861584cd45a76a0d200fb01c15b1ef5d4cf573d163b425c5557947e9", true
	case "zhipu-coding":
		return "crux.catwalk.zhipu-coding", "0.51.23", "7d2ec817fe27c743dc7f3811bc07b00c6106ba6f8800fdb2ead40370082d143f", true
	default:
		return "", "", "", false
	}
}

func MigratedProviderPreset(providerID string) (string, string, bool) {
	id, version, _, migrated := CanonicalMigratedProviderPreset(providerID)
	return id, version, migrated
}
