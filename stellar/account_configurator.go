package stellar

import (
	"net/http"
	"time"

	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/services/bifrost/common"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/support/log"
)

func (ac *AccountConfigurator) Start() error {
	ac.log = common.CreateLogger("StellarAccountConfigurator")
	ac.log.Info("StellarAccountConfigurator starting")

	_, err := keypair.Parse(ac.IssuerPublicKey)
	if err != nil || (err == nil && ac.IssuerPublicKey[0] != 'G') {
		err = errors.Wrap(err, "Invalid IssuerPublicKey")
		ac.log.Error(err)
		return err
	}

	kp, err := keypair.Parse(ac.SignerSecretKey)
	if err != nil || (err == nil && ac.SignerSecretKey[0] != 'S') {
		err = errors.Wrap(err, "Invalid SignerSecretKey")
		ac.log.Error(err)
		return err
	}

	ac.signerPublicKey = kp.Address()

	root, err := ac.Horizon.Root()
	if err != nil {
		err = errors.Wrap(err, "Error loading Horizon root")
		ac.log.Error(err)
		return err
	}

	if root.NetworkPassphrase != ac.NetworkPassphrase {
		return errors.Errorf("Invalid network passphrase (have=%s, want=%s)", root.NetworkPassphrase, ac.NetworkPassphrase)
	}

	err = ac.updateSignerSequence()
	if err != nil {
		err = errors.Wrap(err, "Error loading issuer sequence number")
		ac.log.Error(err)
		return err
	}

	go ac.logStats()
	return nil
}

func (ac *AccountConfigurator) logStats() {
	for {
		ac.log.WithField("currently_processing", ac.processingCount).Info("Stats")
		time.Sleep(15 * time.Second)
	}
}

// ConfigureAccount configures a new account that participated in ICO.
// * First it creates a new account.
// * Once a trusline exists, it credits it with received number of ETH or BTC.
func (ac *AccountConfigurator) ConfigureAccount(destination, assetCode, amount string) {
	localLog := ac.log.WithFields(log.F{
		"destination": destination,
		"assetCode":   assetCode,
		"amount":      amount,
	})
	localLog.Info("Configuring Stellar account")

	ac.processingCountMutex.Lock()
	ac.processingCount++
	ac.processingCountMutex.Unlock()

	defer func() {
		ac.processingCountMutex.Lock()
		ac.processingCount--
		ac.processingCountMutex.Unlock()
	}()

	// Check if account exists. If it is, skip creating it.
	for {
		_, exists, err := ac.getAccount(destination)
		if err != nil {
			localLog.WithField("err", err).Error("Error loading account from Horizon")
			time.Sleep(2 * time.Second)
			continue
		}

		if exists {
			break
		}

		localLog.WithField("destination", destination).Info("Creating Stellar account")
		err = ac.createAccountTransaction(destination)
		if err != nil {
			localLog.WithField("err", err).Error("Error creating Stellar account")
			time.Sleep(2 * time.Second)
			continue
		}

		break
	}

	if ac.OnAccountCreated != nil {
		ac.OnAccountCreated(destination)
	}

	// Wait for signer changes...
	for {
		account, err := ac.Horizon.LoadAccount(destination)
		if err != nil {
			localLog.WithField("err", err).Error("Error loading account to check trustline")
			time.Sleep(2 * time.Second)
			continue
		}

		if ac.signerExistsOnly(account) {
			break
		}

		time.Sleep(2 * time.Second)
	}

	localLog.Info("Signer found")

	// When signer was created we can configure account in Bifrost without requiring
	// the user to share the account's secret key.
	localLog.Info("Sending token")
	err := ac.configureAccountTransaction(destination, assetCode, amount, ac.NeedsAuthorize)
	if err != nil {
		localLog.WithField("err", err).Error("Error configuring an account")
		return
	}

	if ac.LockUnixTimestamp == 0 {
		localLog.Info("Removing temporary signer")
		err = ac.removeTemporarySigner(destination)
		if err != nil {
			localLog.WithField("err", err).Error("Error removing temporary signer")
			return
		}

		if ac.OnExchanged != nil {
			ac.OnExchanged(destination)
		}
	} else {
		localLog.Info("Creating unlock transaction to remove temporary signer")
		transaction, err := ac.buildUnlockAccountTransaction(destination)
		if err != nil {
			localLog.WithField("err", err).Error("Error creating unlock transaction")
			return
		}

		if ac.OnExchangedTimelocked != nil {
			ac.OnExchangedTimelocked(destination, transaction)
		}
	}

	localLog.Info("Account successully configured")
}

func (ac *AccountConfigurator) getAccount(account string) (horizon.Account, bool, error) {
	var hAccount horizon.Account
	hAccount, err := ac.Horizon.LoadAccount(account)
	if err != nil {
		if err, ok := err.(*horizon.Error); ok && err.Response.StatusCode == http.StatusNotFound {
			return hAccount, false, nil
		}
		return hAccount, false, err
	}

	return hAccount, true, nil
}

// signerExistsOnly returns true if account has exactly one signer and it's
// equal to `signerPublicKey`.
func (ac *AccountConfigurator) signerExistsOnly(account horizon.Account) bool {
	tempSignerFound := false

	for _, signer := range account.Signers {
		if signer.PublicKey == ac.signerPublicKey {
			if signer.Weight == 1 {
				tempSignerFound = true
			}
		} else {
			// For each other signer, weight should be equal 0
			if signer.Weight != 0 {
				return false
			}
		}
	}

	return tempSignerFound
}
