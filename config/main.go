package config

type Config struct {
	Port       int  `valid:"required"`
	UsingProxy bool `valid:"optional" toml:"using_proxy"`
	Bitcoin    struct {
		MasterPublicKey string `valid:"required" toml:"master_public_key"`
		// Host only
		RpcServer string `valid:"required" toml:"rpc_server"`
		RpcUser   string `valid:"optional" toml:"rpc_user"`
		RpcPass   string `valid:"optional" toml:"rpc_pass"`
		Testnet   bool   `valid:"optional" toml:"testnet"`
	} `valid:"required" toml:"bitcoin"`
	Ethereum struct {
		NetworkID       string `valid:"required,int" toml:"network_id"`
		MasterPublicKey string `valid:"required" toml:"master_public_key"`
		// Host only
		RpcServer string `valid:"required" toml:"rpc_server"`
	} `valid:"required" toml:"ethereum"`
	Stellar struct {
		Horizon           string `valid:"required" toml:"horizon"`
		NetworkPassphrase string `valid:"required" toml:"network_passphrase"`
		// IssuerPublicKey is public key of the assets issuer or hot wallet.
		IssuerPublicKey string `valid:"required" toml:"issuer_public_key"`
		// SignerSecretKey is:
		// * Issuer's secret key if only one instance of Bifrost is deployed.
		// * Channel's secret key if more than one instance of Bifrost is deployed.
		// https://www.stellar.org/developers/guides/channels.html
		// Signer's sequence number will be consumed in transaction's sequence number.
		SignerSecretKey string `valid:"required" toml:"signer_secret_key"`
	} `valid:"required" toml:"stellar"`
	Database struct {
		Type string `valid:"matches(^postgres$)"`
		DSN  string `valid:"required"`
	} `valid:"required"`
}
