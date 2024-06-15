/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/pincher95/cor/pkg/handlers"
	"github.com/pincher95/cor/pkg/printer"
	"github.com/spf13/cobra"
)

// imagesCmd represents the images command
var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "A brief description of your command",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		region, err := cmd.Flags().GetString("region")
		if err != nil {
			fmt.Println(err)
		}

		authMethod, err := cmd.Flags().GetString("auth-method")
		if err != nil {
			fmt.Println(err)
		}

		profile, err := cmd.Flags().GetString("profile")
		if err != nil {
			fmt.Println(err)
		}

		creationDateBefore, err := cmd.Flags().GetString("creation-date-before")
		if err != nil {
			fmt.Println(err)
		}

		creationDateAfter, err := cmd.Flags().GetString("creation-date-after")
		if err != nil {
			fmt.Println(err)
		}

		imageName, err := cmd.Flags().GetString("filter-by-name")
		if err != nil {
			fmt.Println(err)
		}

		deleteImages, err := cmd.Flags().GetBool("delete-images")
		if err != nil {
			fmt.Println(err)
		}

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		ec2Client := ec2.NewFromConfig(*cfg)

		stsClient := sts.NewFromConfig(*cfg)

		ownerID, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
		if err != nil {
			fmt.Println(err)
		}

		beforeCreationDate, err := time.Parse(time.RFC3339, fmt.Sprintf("%sT00:00:00Z", creationDateBefore))
		if err != nil {
			fmt.Println(err)
			return
		}

		afterCreationDate, err := time.Parse(time.RFC3339, fmt.Sprintf("%sT00:00:00Z", creationDateAfter))
		if err != nil {
			fmt.Println(err)
			return
		}

		var wg sync.WaitGroup

		tableRows := make([]table.Row, 0)

		// Create a channel to receive images information
		imagesChan := make(chan types.Image, 100)
		// Start a goroutine to fetch images and send them to the channel
		go func() {
			wg.Add(1)
			defer wg.Done()
			defer close(imagesChan)
			err := describeImages(context.TODO(), ec2Client, imagesChan, ownerID.Account, &imageName, beforeCreationDate, afterCreationDate)
			if err != nil {
				fmt.Println(err)
			}
		}()

		for image := range imagesChan {
			snapshotIds := getSnapshotIds(image)
			instanceChan := make(chan types.Instance, 100)

			wg.Add(1)
			// Start a goroutine to fetch instances and send them to the channel
			go func(imageID *string) {
				defer wg.Done()
				err := describeInstances(context.TODO(), ec2Client, instanceChan, imageID)
				if err != nil {
					fmt.Println(err)
				}
			}(image.ImageId)

			go func() {
				wg.Wait()
				close(instanceChan)
			}()

			usedByInstances := getInstances(instanceChan, image.ImageId)
			tableRows = append(tableRows, table.Row{*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"), strings.Join(usedByInstances, "\n")})
		}

		columnConfig := []table.ColumnConfig{
			{Name: "ami name", AlignHeader: text.AlignCenter},
			{Name: "ami id", AlignHeader: text.AlignCenter},
			{Name: "creation date", AlignHeader: text.AlignCenter},
			{Name: "snapshot ids", AlignHeader: text.AlignCenter},
			{Name: "used by Instance", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}

		if deleteImages {
			reader := bufio.NewReader(os.Stdin)
			fmt.Print("Are you sure you want to proceed? (yes/no): ")
			response, _ := reader.ReadString('\n')
			response = strings.ToLower(strings.TrimSpace(response))

			if response == "yes" {
				for _, tableRow := range tableRows {
					_, err := ec2Client.DeregisterImage(context.TODO(), &ec2.DeregisterImageInput{
						ImageId: aws.String(tableRow[1].(string)),
						DryRun:  aws.Bool(!deleteImages),
					})
					if err != nil {
						fmt.Println(err)
					}

					for _, snapshot := range strings.Split(tableRow[3].(string), "\n") {
						_, err := ec2Client.DeleteSnapshot(context.TODO(), &ec2.DeleteSnapshotInput{
							SnapshotId: aws.String(snapshot),
							DryRun:     aws.Bool(!deleteImages),
						})
						if err != nil {
							fmt.Println(err)
						}
					}
				}

			} else if response == "no" {
				fmt.Println("Aborted.")
				// Your code to handle aborting here
			} else {
				fmt.Println("Invalid response. Please enter 'yes' or 'no'.")
			}
		}
	},
}

func init() {
	// rootCmd.AddCommand(imagesCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	imagesCmd.PersistentFlags().String("creation-date-before", time.Now().UTC().Format("2006-01-02"), "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	imagesCmd.PersistentFlags().String("creation-date-after", time.Now().UTC().Format("2006-01-02"), "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// imagesCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	imagesCmd.Flags().String("filter-by-name", "*", "The name of the AMI (provided during image creation) ,You can use a wildcard ( * ).")

	imagesCmd.Flags().Bool("delete-images", false, "")
}

func describeInstances(ctx context.Context, client *ec2.Client, instanceChan chan<- types.Instance, instanceImageID *string) error {
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("image-id"),
				Values: []string{*instanceImageID},
			},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				instanceChan <- instance
			}
		}
	}
	return nil
}

func describeImages(ctx context.Context, client *ec2.Client, imagesChan chan<- types.Image, ownerID, imageName *string, beforeCreationDate, afterCreationDate time.Time) error {
	paginator := ec2.NewDescribeImagesPaginator(client, &ec2.DescribeImagesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("owner-id"),
				Values: []string{*ownerID},
			},
			{
				Name:   aws.String("name"),
				Values: []string{*imageName},
			},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}

		for _, image := range page.Images {
			imageCreationDate, err := time.Parse(time.RFC3339, *image.CreationDate)
			if err != nil {
				return err
			}

			if beforeCreationDate.After(afterCreationDate) {
				if imageCreationDate.After(afterCreationDate) && imageCreationDate.Before(beforeCreationDate) {
					imagesChan <- image
				}
			} else if beforeCreationDate.Before(afterCreationDate) {
				if imageCreationDate.Before(beforeCreationDate) || imageCreationDate.After(afterCreationDate) {
					imagesChan <- image
				}
			} else if beforeCreationDate.Equal(afterCreationDate) {
				if imageCreationDate.Equal(beforeCreationDate) {
					imagesChan <- image
				}
			}
		}
	}
	return nil
}

func getSnapshotIds(image types.Image) []string {
	snapshotIds := make([]string, 0, len(image.BlockDeviceMappings))
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs != nil {
			snapshotIds = append(snapshotIds, *mapping.Ebs.SnapshotId)
		}
	}
	return snapshotIds
}

func getInstances(instanceChan <-chan types.Instance, imageID *string) []string {
	usedByInstances := make([]string, 0)
	for instance := range instanceChan {
		if *instance.ImageId == *imageID {
			usedByInstances = append(usedByInstances, *instance.InstanceId)
		}
	}
	return usedByInstances
}
