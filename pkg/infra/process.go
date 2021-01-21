package infra

import (
	"fmt"
	"time"
	"io/ioutil"
	"os/exec"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)


var Cores = 4

func Process(configPath string, phases string, num int, burst int, rate float64, logger *log.Logger) error {
	switch phases {
	case "endorserOnly":
		return EndorserOnly(configPath, num, logger)
	case "mockOrdererOnly":
		return MockOrdererOnly(configPath, num, logger)
	case "ordererOnly":
		return OrdererOnly(configPath, num, logger)
	case "ordererAndCommitter":
		return OrdererAndCommitter(configPath, num, logger)
	default:
		return AllPhases(configPath, num, burst, rate, logger)
	}

}

func AllPhases(configPath string, num int,burst int, rate float64, logger *log.Logger) error {

	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	crypto, err := config.LoadCrypto()
	if err != nil {
		return err
	}
	raw := make(chan *Elements, burst)
	signed := make([]chan *Elements, len(config.Endorsers))

	endorsed := make(chan *Elements, 10)
	envs := make(chan *Elements, 10)

	done := make(chan struct{})
	finishCh := make(chan struct{})
	errorCh := make(chan error, burst)
	assember := &Assembler{Signer: crypto}
	blockCollector, err := NewBlockCollector(config.CommitThreshold, len(config.Committers))
	if err != nil {
		return errors.Wrap(err, "failed to create block collector")
	}
	for i := 0; i < len(config.Endorsers); i++ {
		signed[i] = make(chan *Elements, burst)
	}

	for i := 0; i < 2*Cores; i++ {
		go assember.StartSigner(raw, signed, errorCh, done)
		go assember.StartIntegrator(endorsed, envs, errorCh, done)
	}

	fmt.Printf("config.NumofConn: %d", config.NumOfConn)
	proposers, err := CreateProposers(config.NumOfConn, config.ClientPerConn, config.Endorsers, logger)
	if err != nil {
		return err
	}
	proposers.Start(signed, endorsed, done, config)

	broadcasters, err := CreateBroadcasters(config.NumOfConn, config.Orderer, logger)
	if err != nil {
		return err
	}
	broadcasters.Start(envs, errorCh, done)

	observers, err := CreateObservers(config.Channel, config.Committers, crypto, logger)
	if err != nil {
		return err
	}

	start := time.Now()

	go observers.Start(num, errorCh, finishCh, start, blockCollector)
	go StartCreateProposal(num, burst, rate, config, crypto, raw, errorCh, logger)

	for {
		select {
		case err = <-errorCh:
			return err
		case <-finishCh:
			duration := time.Since(start)
			close(done)

			logger.Infof("Completed processing transactions.")
			fmt.Printf("tx: %d, duration: %+v, tps: %f\n", num, duration, float64(num)/duration.Seconds())
			return nil
		}
	}
}

func EndorserOnly(configPath string, num int, logger *log.Logger) error {
	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	crypto, err := config.LoadCrypto()
	if err != nil {
		return err
	}
	signed := make([]chan *Elements, len(config.Endorsers))
	endorsed := make(chan *Elements, num)
	done := make(chan struct{})
	finishCh := make(chan struct{})
	errorCh := make(chan error, 10)
	assember := &Assembler{Signer: crypto}

	for i := 0; i < len(config.Endorsers); i++ {
		signed[i] = make(chan *Elements, num)
	}

	for i := 0; i < num; i++ {
		prop, err := CreateProposal(
			crypto,
			config.Channel,
			config.Chaincode,
			config.Version,
			config.Args...,
		)
		if err != nil {
			errorCh <- errors.Wrapf(err, "error creating proposal")
		}
		signedProposal, err := assember.sign(&Elements{Proposal: prop})

		for _, v := range signed {
			v <- signedProposal
		}
	}

	for i := 10; i > 0; i-- {
		logger.Infof("test will begin in %ds \n", i)
		time.Sleep(1 * time.Second)
	}

	proposers, err := CreateProposers(config.NumOfConn, config.ClientPerConn, config.Endorsers, logger)
	if err != nil {
		return err
	}

	endorserObserver := CreateEndorserObserver(endorsed, logger)
	if err != nil {
		return err
	}

	start := time.Now()
	proposers.Start(signed, endorsed, done, config)
	go endorserObserver.Start(num, finishCh, start)

	for {
		select {
		case err = <-errorCh:
			return err
		case <-finishCh:
			duration := time.Since(start)
			close(done)

			logger.Infof("Completed processing transactions.")
			logger.Infof("tx: %d, duration: %+v, tps: %f\n", num, duration, float64(num)/duration.Seconds())
			return nil
		}
	}
}

func MockOrdererOnly(configPath string, num int, logger *log.Logger) error {
	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	crypto, err := config.LoadCrypto()
	if err != nil {
		return err
	}
	envs := make(chan *Elements, num)
	done := make(chan struct{})
	finishCh := make(chan struct{})
	errorCh := make(chan error, num)

	broadcasters, err := CreateBroadcasters(config.NumOfConn, config.Orderer, logger)
	if err != nil {
		return err
	}

	ordererObserver := CreateOrdererObserver(config.Channel, config.Orderer, crypto, logger)
	if err != nil {
		return err
	}

	for i := 0; i < num; i++ {
		nonce := []byte("nonce-abc-12345")
		creator, _ := crypto.Serialize()
		txid := protoutil.ComputeTxID(nonce, creator)

		txType := common.HeaderType_ENDORSER_TRANSACTION
		chdr := &common.ChannelHeader{
			Type:      int32(txType),
			ChannelId: config.Channel,
			TxId:      txid,
			Epoch:     uint64(0),
		}

		shdr := &common.SignatureHeader{
			Creator: creator,
			Nonce:   nonce,
		}

		payload := &common.Payload{
			Header: &common.Header{
				ChannelHeader:   protoutil.MarshalOrPanic(chdr),
				SignatureHeader: protoutil.MarshalOrPanic(shdr),
			},
			Data: []byte("data"),
		}
		payloadBytes, _ := protoutil.GetBytesPayload(payload)

		signature, _ := crypto.Sign(payloadBytes)

		envelope := &common.Envelope{
			Payload:   payloadBytes,
			Signature: signature,
		}

		envs <- &Elements{Envelope: envelope}

	}

	for i := 10; i > 0; i-- {
		logger.Infof("test will begin in %ds \n", i)
		time.Sleep(1 * time.Second)
	}

	start := time.Now()
	go ordererObserver.Start(num, errorCh, finishCh, start)
	broadcasters.Start(envs, errorCh, done)

	for {
		select {
		case err = <-errorCh:
			return err
		case <-finishCh:
			duration := time.Since(start)
			close(done)

			logger.Infof("Completed processing transactions.")
			logger.Infof("tx: %d, duration: %+v, tps: %f", num, duration, float64(num)/duration.Seconds())
			return nil
		}
	}
}

func OrdererOnly(configPath string, num int, logger *log.Logger) error {

	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	crypto, err := config.LoadCrypto()
	if err != nil {
		return err
	}
	signed := make([]chan *Elements, len(config.Endorsers))
	endorsed := make(chan *Elements, num)
	done := make(chan struct{})
	finishCh := make(chan struct{})
	errorCh := make(chan error, 10)
	envs := make(chan *Elements, num)
	assember := &Assembler{Signer: crypto}

	for i := 0; i < len(config.Endorsers); i++ {
		signed[i] = make(chan *Elements, num)
	}

	for i := 0; i < num; i++ {
		prop, err := CreateProposal(
			crypto,
			config.Channel,
			config.Chaincode,
			config.Version,
			config.Args...,
		)
		if err != nil {
			errorCh <- errors.Wrapf(err, "error creating proposal")
		}
		signedProposal, err := assember.sign(&Elements{Proposal: prop})

		for _, v := range signed {
			v <- signedProposal
		}
	}

	proposers, err := CreateProposers(config.NumOfConn, config.ClientPerConn, config.Endorsers, logger)
	if err != nil {
		return err
	}

	go proposers.Start(signed, endorsed, done, config)

	for i := 0; i < num; i++ {
		p := <-endorsed
		e, err := assember.assemble(p)
		if err != nil {
			errorCh <- err
		}
		envs <- e
	}

	broadcasters, err := CreateBroadcasters(config.NumOfConn, config.Orderer, logger)
	if err != nil {
		return err
	}

	ordererObserver := CreateOrdererObserver(config.Channel, config.Orderer, crypto, logger)
	if err != nil {
		return err
	}

	for i := 20; i > 0; i-- {
		logger.Infof("test will begin in %ds \n", i)
		time.Sleep(1 * time.Second)
	}

	start := time.Now()

	go ordererObserver.Start(num, errorCh, finishCh, start)
	broadcasters.Start(envs, errorCh, done)

	for {
		select {
		case err = <-errorCh:
			return err
		case <-finishCh:
			duration := time.Since(start)
			close(done)

			logger.Infof("Completed processing transactions.")
			logger.Infof("tx: %d, duration: %+v, tps: %f", num, duration, float64(num)/duration.Seconds())
			return nil
		}
	}

}

func KillPeers(){
  cmd := exec.Command("/bin/bash", "-c", `docker kill peer0.org2.example.com; docker kill peer0.org2.example.com`)
 
  stdout, err := cmd.StdoutPipe()
  if err != nil {
   fmt.Printf("Error:can not obtain stdout pipe for command:%s\n", err)
   return
  }
 
  if err := cmd.Start(); err != nil {
   fmt.Println("Error:The command is err,", err)
   return
  }
 
  bytes, err := ioutil.ReadAll(stdout)
  if err != nil {
   fmt.Println("ReadAll Stdout:", err.Error())
   return
  }
 
  if err := cmd.Wait(); err != nil {
   fmt.Println("wait:", err.Error())
   return
  }
  fmt.Printf("stdout:\n\n %s", bytes)
}

func OrdererAndCommitter(configPath string, num int, logger *log.Logger) error {

	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	crypto, err := config.LoadCrypto()
	if err != nil {
		return err
	}
	signed := make([]chan *Elements, len(config.Endorsers))
	endorsed := make(chan *Elements, num)
	done := make(chan struct{})
	finishCh := make(chan struct{})
	errorCh := make(chan error, 10)
	envs := make(chan *Elements, num)
	assember := &Assembler{Signer: crypto}

	for i := 0; i < len(config.Endorsers); i++ {
		signed[i] = make(chan *Elements, num)
	}

	for i := 0; i < num; i++ {
		prop, err := CreateProposal(
			crypto,
			config.Channel,
			config.Chaincode,
			config.Version,
			config.Args...,
		)
		if err != nil {
			errorCh <- errors.Wrapf(err, "error creating proposal")
		}
		signedProposal, err := assember.sign(&Elements{Proposal: prop})

		for _, v := range signed {
			v <- signedProposal
		}
	}

	proposers, err := CreateProposers(config.NumOfConn, config.ClientPerConn, config.Endorsers, logger)
	if err != nil {
		return err
	}

	go proposers.Start(signed, endorsed, done, config)

	for i := 0; i < num; i++ {
		p := <-endorsed
		e, err := assember.assemble(p)
		if err != nil {
			errorCh <- err
		}
		envs <- e
	}

	broadcasters, err := CreateBroadcasters(config.NumOfConn, config.Orderer, logger)
	if err != nil {
		return err
	}

	for i := 10; i > 0; i-- {
		logger.Infof("test will begin in %ds \n", i)
		time.Sleep(1 * time.Second)
	}

	observer, err := CreateObservers(config.Channel, config.Committers, crypto, logger)
	if err != nil {
		return err
	}

	blockCollector, err := NewBlockCollector(config.CommitThreshold, len(config.Committers))

	start := time.Now()

	broadcasters.Start(envs, errorCh, done)


	go observer.Start(num, errorCh, finishCh, start, blockCollector)
	for {
		select {
		case err = <-errorCh:
			return err
		case <-finishCh:
			duration := time.Since(start)
			close(done)

			logger.Infof("Completed processing transactions.")
			logger.Infof("tx: %d, duration: %+v, tps: %f", num, duration, float64(num)/duration.Seconds())
			return nil
		}
	}

}
